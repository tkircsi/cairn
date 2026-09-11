# syntax=docker/dockerfile:1

FROM golang:bookworm AS build
WORKDIR /src

# go.mod may request a newer toolchain than the image tag; let Go fetch it.
ENV GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cairnd /cairnd
USER nonroot:nonroot
EXPOSE 5050
VOLUME ["/data"]
ENTRYPOINT ["/cairnd"]
CMD ["-addr", "0.0.0.0:5050", "-root", "/data"]
