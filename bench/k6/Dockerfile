FROM golang:1.25-bookworm AS bench-tools

WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/benchfixture ./cmd/benchfixture \
  && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/servicebench ./cmd/servicebench

FROM grafana/k6:0.54.0

USER root
WORKDIR /work
COPY bench/k6/ /work/bench/
COPY --from=bench-tools /out/benchfixture /usr/local/bin/benchfixture
COPY --from=bench-tools /out/servicebench /usr/local/bin/servicebench
USER k6

ENTRYPOINT ["k6"]
