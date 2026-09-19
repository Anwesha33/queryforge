# pg_query_go embeds libpg_query, which is C. That makes cgo mandatory here —
# the whole point of this service is to parse SQL with the server's own grammar,
# and there is no pure-Go equivalent that is actually the same parser.
FROM golang:1.24 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGET=api
RUN CGO_ENABLED=1 go build -trimpath -o /out/app ./cmd/${TARGET}

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -u 10001 -m queryforge
USER queryforge
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/app"]
