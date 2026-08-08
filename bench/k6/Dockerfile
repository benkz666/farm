FROM grafana/k6:0.54.0

USER root
WORKDIR /work
COPY bench/k6/ /work/bench/
USER k6

ENTRYPOINT ["k6"]
