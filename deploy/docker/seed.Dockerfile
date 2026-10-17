# Image Job seed: psql + seed/*.sql + seed/env/<env>/*.sql (dipilih lewat KOTOKI_ENV).
FROM postgres:17-alpine
COPY seed /seed
COPY deploy/docker/seed.sh /usr/local/bin/seed.sh
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/seed.sh"]
