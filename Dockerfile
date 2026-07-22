FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/inno-live-server ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --system --uid 65532 --create-home innolive
COPY --from=build /out/inno-live-server /inno-live-server
USER 65532
EXPOSE 8000/udp
EXPOSE 8000/tcp
EXPOSE 50000-60000/udp
ENTRYPOINT ["/inno-live-server"]
