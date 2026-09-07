// Command referrers measures how a registry's cost changes as referrers accumulate
// on a single live subject.
//
// The workload is not synthetic for its own sake. It is the shape a scan or signing
// pipeline produces: one record, re-scanned on a schedule, each run pushing a report
// whose bytes differ because they carry a timestamp -- so a new digest, a new
// manifest, and one more referrer on a subject that is never deleted. Nothing
// collects them, and a client asking "what has been said about this record" gets all
// of them. That makes referrers-per-subject an axis that only grows in production,
// which is why it is worth knowing what it costs.
//
// Three measurements, taken against the same registry over one growing subject:
//
//	push     PUT of the referrer manifest, which is where a registry that maintains
//	         a per-repository index does its work
//	fallback the extra work a registry without a referrers API forces on the client
//	query    GET of the referrers list, which is the read the accumulation degrades
//	control  HEAD of the subject manifest, unrelated to referrers, included to show
//	         whether the cost leaks into operations that have nothing to do with them
//
// The fallback column is why this measures a client rather than an endpoint. A
// registry with no referrers API is not thereby free of the cost: the spec makes the
// client maintain an image index under a tag derived from the subject, which means
// fetching that index, appending to it, and pushing it back on every single push. The
// document being rewritten grows by one descriptor each time, so the work is linear in
// what has accumulated and the total is quadratic -- and it is paid whether or not
// anyone ever asks for the list. Timing only the endpoints would report that registry
// as the fastest of the three by leaving out everything it delegates.
//
// So the driver probes end-12 and takes the branch a conformant client would, which is
// what makes the three numbers comparable: each is the cost of recording one referrer
// and of listing them, against a registry behaving as it really does.
//
// Everything is plain HTTP so that cairn, Zot and Distribution are driven by exactly
// the same bytes in exactly the same order. Sequential by design: this is a scaling
// curve, and adding concurrency would mix contention into a measurement of work.
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func main() {
	var (
		registry = flag.String("registry", "http://127.0.0.1:5050", "registry root URL")
		repo     = flag.String("repo", "bench/records", "repository to push into")
		total    = flag.Int("n", 2000, "referrers to push onto one subject")
		every    = flag.Int("sample-every", 50, "sample query and control latency every N referrers")
		repeats  = flag.Int("repeats", 5, "measurements per sample point, of which the median is kept")
		out      = flag.String("out", "", "CSV output path (default stdout)")
		label    = flag.String("label", "", "registry name recorded in the output")
	)

	flag.Parse()

	r := &driver{
		root: *registry,
		repo: *repo,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			// Connection reuse matters: without it every request pays a TCP handshake, which
			// is noise of the same order as the fast registries' answers.
			Transport: &http.Transport{MaxIdleConnsPerHost: 4},
		},
	}

	if err := r.run(*label, *total, *every, *repeats, *out); err != nil {
		log.Fatal(err)
	}
}

type driver struct {
	root   string
	repo   string
	client *http.Client

	// empty is the shared config blob every referrer points at, pushed once.
	empty ocispec.Descriptor

	// native records whether the registry answered end-12, decided by probing it once.
	// When it does not, every push additionally maintains the fallback index.
	native bool
}

// sample is one row of the curve.
type sample struct {
	n            int
	pushMillis   float64
	fallbackMs   float64
	queryMillis  float64
	controlMs    float64
	responseSize int
	returned     int
}

func (d *driver) run(label string, total, every, repeats int, out string) error {
	// The subject: an ordinary image manifest, pushed once. Nothing about it changes as
	// referrers accumulate, which is what makes the control measurement meaningful.
	subject, err := d.pushSubject()
	if err != nil {
		return fmt.Errorf("push subject: %w", err)
	}

	// Which branch to take is the registry's answer to give, not a flag to set. The spec
	// defines the probe -- a registry supporting the API returns 200 with an index even
	// when there are no referrers, precisely so that a 404 is unambiguous -- and taking
	// the branch it indicates is what makes this a measurement of a client rather than of
	// an endpoint that may not exist.
	if err := d.probe(subject.Digest); err != nil {
		return err
	}

	mode := "referrers tag schema (no end-12)"
	if d.native {
		mode = "native end-12"
	}

	fmt.Fprintf(os.Stderr, "%s: subject %s, %s\n", label, subject.Digest, mode)

	samples := make([]sample, 0, total/every+1)

	for i := 1; i <= total; i++ {
		pushed, fallback, err := d.pushReferrer(subject, i)
		if err != nil {
			return fmt.Errorf("push referrer %d: %w", i, err)
		}

		if i%every != 0 && i != total {
			continue
		}

		queryMs, size, returned, err := d.medianQuery(subject.Digest, repeats)
		if err != nil {
			return fmt.Errorf("query at %d: %w", i, err)
		}

		controlMs, err := d.medianControl(subject.Digest, repeats)
		if err != nil {
			return fmt.Errorf("control at %d: %w", i, err)
		}

		// Correctness before speed. A registry returning fewer referrers than were pushed
		// is answering a different question, and its timings are not comparable -- this is
		// how a silently paginating or lossy implementation gets caught rather than
		// credited with being fast.
		if returned != i {
			return fmt.Errorf("at %d referrers the registry returned %d", i, returned)
		}

		samples = append(samples, sample{
			n:            i,
			pushMillis:   pushed,
			fallbackMs:   fallback,
			queryMillis:  queryMs,
			controlMs:    controlMs,
			responseSize: size,
			returned:     returned,
		})

		fmt.Fprintf(os.Stderr,
			"%s n=%-6d push=%7.2fms fallback=%8.2fms query=%8.2fms control=%6.2fms body=%dKB\n",
			label, i, pushed, fallback, queryMs, controlMs, size/1024)
	}

	return writeCSV(out, label, d.native, samples)
}

// pushSubject creates the manifest that everything else will refer to.
func (d *driver) pushSubject() (ocispec.Descriptor, error) {
	config, err := d.pushBlob([]byte(`{"subject":true}`))
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	config.MediaType = ocispec.MediaTypeImageConfig

	layer, err := d.pushBlob([]byte("the record itself"))
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	layer.MediaType = ocispec.MediaTypeImageLayer

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	}

	raw, err := json.Marshal(manifest)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	dgst := digest.FromBytes(raw)

	if _, err := d.putManifest(dgst, raw); err != nil {
		return ocispec.Descriptor{}, err
	}

	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    dgst,
		Size:      int64(len(raw)),
	}, nil
}

// probe decides which branch this registry requires, using the test the spec defines
// for it: a 200 means end-12 is implemented, a 404 means it is not and the client owns
// the fallback index.
func (d *driver) probe(subject digest.Digest) error {
	url := fmt.Sprintf("%s/v2/%s/referrers/%s", d.root, d.repo, subject)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	status, body, err := d.do(req)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK:
		d.native = true
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		d.native = false
		return nil
	default:
		return fmt.Errorf("referrers probe returned %d: %s", status, truncate(body))
	}
}

// fallbackTag is the tag a client maintains when the registry has no referrers API:
// the subject's digest with its separator replaced, since a tag cannot contain a colon.
func fallbackTag(subject digest.Digest) string {
	return fmt.Sprintf("%s-%s", subject.Algorithm(), subject.Encoded())
}

// pushReferrer pushes one scan-report-shaped referrer, returning the manifest PUT
// latency and the fallback-index latency, both in milliseconds.
//
// Only the manifest PUT is counted as push. The blob uploads before it are identical
// work for every registry -- bytes to disk -- whereas the PUT is where a registry that
// keeps a per-repository index has to update it, so timing them together would dilute
// the thing being measured.
func (d *driver) pushReferrer(subject ocispec.Descriptor, i int) (float64, float64, error) {
	// Unique bytes per referrer, which is the whole reason these accumulate: the report
	// embeds the time it was produced, so re-scanning the same record yields a different
	// digest rather than a no-op.
	report := fmt.Sprintf(`{"scannedAt":%q,"run":%d,"safe":true}`,
		time.Now().UTC().Format(time.RFC3339Nano), i)

	layer, err := d.pushBlob([]byte(report))
	if err != nil {
		return 0, 0, err
	}

	layer.MediaType = "application/vnd.example.scanreport.v1+json"

	config, err := d.emptyConfig()
	if err != nil {
		return 0, 0, err
	}

	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.scanreport.v1+json",
		Config:       config,
		Layers:       []ocispec.Descriptor{layer},
		Subject:      &subject,
		Annotations: map[string]string{
			"org.example.run":     strconv.Itoa(i),
			"org.example.scanner": "bench",
		},
	}

	raw, err := json.Marshal(manifest)
	if err != nil {
		return 0, 0, err
	}

	dgst := digest.FromBytes(raw)

	pushMs, err := d.putManifest(dgst, raw)
	if err != nil {
		return 0, 0, err
	}

	if d.native {
		return pushMs, 0, nil
	}

	// The registry does not track subjects, so the client has to. This is the read the
	// manifest push should have made unnecessary, and its cost is the reason a registry
	// without end-12 is not the cheap option it looks like.
	fallbackMs, err := d.appendToFallbackIndex(subject, ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		Digest:       dgst,
		Size:         int64(len(raw)),
		ArtifactType: manifest.ArtifactType,
		Annotations:  manifest.Annotations,
	})
	if err != nil {
		return 0, 0, err
	}

	return pushMs, fallbackMs, nil
}

// appendToFallbackIndex performs the read-modify-write the referrers tag schema
// requires, and returns how long all of it took.
//
// Three requests, of which two carry the whole accumulated list: fetch the index,
// append one descriptor, push it back. The index grows by a descriptor per referrer, so
// each push moves and re-parses everything pushed before it -- linear per push,
// quadratic over the run. The spec notes this is also racy, since two clients that read
// the same index will each write back a version missing the other's entry, and there is
// no way to make it atomic from outside the registry.
func (d *driver) appendToFallbackIndex(
	subject ocispec.Descriptor,
	referrer ocispec.Descriptor,
) (float64, error) {
	tag := fallbackTag(subject.Digest)
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", d.root, d.repo, tag)

	start := time.Now()

	get, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	get.Header.Set("Accept", ocispec.MediaTypeImageIndex)

	status, body, err := d.do(get)
	if err != nil {
		return 0, err
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
	}

	switch status {
	case http.StatusOK:
		if err := json.Unmarshal(body, &index); err != nil {
			return 0, fmt.Errorf("decode fallback index: %w", err)
		}
	case http.StatusNotFound:
		// First referrer for this subject; the tag does not exist yet.
	default:
		return 0, fmt.Errorf("fetch fallback index: status %d: %s", status, truncate(body))
	}

	index.Manifests = append(index.Manifests, referrer)

	raw, err := json.Marshal(index)
	if err != nil {
		return 0, err
	}

	put, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}

	put.Header.Set("Content-Type", ocispec.MediaTypeImageIndex)
	put.ContentLength = int64(len(raw))

	status, body, err = d.do(put)
	if err != nil {
		return 0, err
	}

	if status != http.StatusCreated {
		return 0, fmt.Errorf("push fallback index: status %d: %s", status, truncate(body))
	}

	return millis(time.Since(start)), nil
}

// pushBlob uploads bytes as a two-request session: POST to open, PUT to close.
//
// The single-request form (end-4b) would be less work, but Distribution answers it
// with a 202 and a session to finish rather than a 201 -- the spec makes it a MAY. The
// session flow is the one all three implement the same way, and using it everywhere
// keeps the bytes on the wire identical per registry, which matters more here than
// saving a round trip that is not being timed.
func (d *driver) pushBlob(data []byte) (ocispec.Descriptor, error) {
	dgst := digest.FromBytes(data)

	open, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v2/%s/blobs/uploads/", d.root, d.repo), nil)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	resp, err := d.client.Do(open)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return ocispec.Descriptor{}, fmt.Errorf("open upload status %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return ocispec.Descriptor{}, fmt.Errorf("open upload returned no Location")
	}

	// The spec allows the Location to be relative, and Distribution sends it that way
	// while cairn sends an absolute URL. Resolving against the request handles both
	// without either being special-cased.
	target, err := resp.Request.URL.Parse(location)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("bad Location %q: %w", location, err)
	}

	query := target.Query()
	query.Set("digest", dgst.String())
	target.RawQuery = query.Encode()

	closeReq, err := http.NewRequest(http.MethodPut, target.String(), bytes.NewReader(data))
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	closeReq.Header.Set("Content-Type", "application/octet-stream")
	closeReq.ContentLength = int64(len(data))

	status, body, err := d.do(closeReq)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	if status != http.StatusCreated {
		return ocispec.Descriptor{}, fmt.Errorf("close upload status %d: %s", status, truncate(body))
	}

	return ocispec.Descriptor{Digest: dgst, Size: int64(len(data))}, nil
}

// emptyConfig pushes the shared empty config blob once and remembers it. Every
// referrer names the same one, so re-pushing it per iteration would add a round trip
// to each push for no reason and put avoidable variance in the timings.
func (d *driver) emptyConfig() (ocispec.Descriptor, error) {
	if d.empty.Digest != "" {
		return d.empty, nil
	}

	descriptor, err := d.pushBlob([]byte("{}"))
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	descriptor.MediaType = ocispec.MediaTypeEmptyJSON
	d.empty = descriptor

	return d.empty, nil
}

func (d *driver) putManifest(dgst digest.Digest, raw []byte) (float64, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", d.root, d.repo, dgst)

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
	req.ContentLength = int64(len(raw))

	start := time.Now()

	status, _, err := d.do(req)

	elapsed := millis(time.Since(start))

	if err != nil {
		return 0, err
	}

	if status != http.StatusCreated {
		return 0, fmt.Errorf("manifest push status %d", status)
	}

	return elapsed, nil
}

// queryReferrers asks for the list the way this registry allows, and returns its
// latency, body size and count.
//
// Native or fallback, the client is asking one question and gets back an index of the
// same descriptors, which is what makes the two timings comparable even though they are
// different requests. The difference is who assembled the answer.
func (d *driver) queryReferrers(subject digest.Digest) (float64, int, int, error) {
	url := fmt.Sprintf("%s/v2/%s/referrers/%s", d.root, d.repo, subject)
	accept := ocispec.MediaTypeImageIndex

	if !d.native {
		url = fmt.Sprintf("%s/v2/%s/manifests/%s", d.root, d.repo, fallbackTag(subject))
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}

	req.Header.Set("Accept", accept)

	start := time.Now()

	status, body, err := d.do(req)

	elapsed := millis(time.Since(start))

	if err != nil {
		return 0, 0, 0, err
	}

	if status != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("referrers status %d: %s", status, truncate(body))
	}

	var index ocispec.Index
	if err := json.Unmarshal(body, &index); err != nil {
		return 0, 0, 0, fmt.Errorf("decode referrers: %w", err)
	}

	return elapsed, len(body), len(index.Manifests), nil
}

// control is a HEAD of the subject, which has nothing to do with referrers. If its
// cost tracks the referrer count, the accumulation is not contained.
func (d *driver) control(subject digest.Digest) (float64, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", d.root, d.repo, subject)

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}

	// Distribution answers 404 for a manifest whose media type the client did not say it
	// accepts, which is legal and is how content negotiation is supposed to work -- cairn
	// and Zot are the lenient ones here. Stating it explicitly is what a real client does.
	req.Header.Set("Accept", strings.Join([]string{
		ocispec.MediaTypeImageManifest,
		ocispec.MediaTypeImageIndex,
	}, ", "))

	start := time.Now()

	status, _, err := d.do(req)

	elapsed := millis(time.Since(start))

	if err != nil {
		return 0, err
	}

	if status != http.StatusOK {
		return 0, fmt.Errorf("control status %d", status)
	}

	return elapsed, nil
}

// medianQuery repeats the read and keeps the median, because a single timing at a
// sample point is one scheduling accident away from being the headline.
func (d *driver) medianQuery(subject digest.Digest, repeats int) (float64, int, int, error) {
	times := make([]float64, 0, repeats)

	var size, returned int

	for range repeats {
		ms, gotSize, gotReturned, err := d.queryReferrers(subject)
		if err != nil {
			return 0, 0, 0, err
		}

		times = append(times, ms)
		size, returned = gotSize, gotReturned
	}

	return median(times), size, returned, nil
}

func (d *driver) medianControl(subject digest.Digest, repeats int) (float64, error) {
	times := make([]float64, 0, repeats)

	for range repeats {
		ms, err := d.control(subject)
		if err != nil {
			return 0, err
		}

		times = append(times, ms)
	}

	return median(times), nil
}

// do runs a request and drains the body, since a response left unread is a
// connection not reused and a duration not fully accounted for.
func (d *driver) do(req *http.Request) (int, []byte, error) {
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}

	return resp.StatusCode, body, nil
}

func writeCSV(path, label string, native bool, samples []sample) error {
	sink := os.Stdout

	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}

		defer f.Close()

		sink = f
	}

	w := csv.NewWriter(sink)
	defer w.Flush()

	if err := w.Write([]string{
		"registry", "mode", "referrers",
		"push_ms", "fallback_ms", "query_ms", "control_ms", "body_bytes",
	}); err != nil {
		return err
	}

	mode := "fallback"
	if native {
		mode = "native"
	}

	for _, s := range samples {
		row := []string{
			label,
			mode,
			strconv.Itoa(s.n),
			strconv.FormatFloat(s.pushMillis, 'f', 3, 64),
			strconv.FormatFloat(s.fallbackMs, 'f', 3, 64),
			strconv.FormatFloat(s.queryMillis, 'f', 3, 64),
			strconv.FormatFloat(s.controlMs, 'f', 3, 64),
			strconv.Itoa(s.responseSize),
		}

		if err := w.Write(row); err != nil {
			return err
		}
	}

	return nil
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	mid := len(sorted) / 2

	if len(sorted)%2 == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / 2
}

func millis(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func truncate(body []byte) string {
	const limit = 200

	if len(body) > limit {
		return string(body[:limit]) + "..."
	}

	return string(body)
}
