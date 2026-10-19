# Build context: deploy/
FROM prom/prometheus:v3.5.0
COPY railway/prometheus.yml /etc/prometheus/prometheus.yml
COPY prometheus/alerts.yml /etc/prometheus/alerts.yml
CMD ["--config.file=/etc/prometheus/prometheus.yml", \
     "--storage.tsdb.path=/prometheus", \
     "--storage.tsdb.retention.time=2d", \
     "--web.listen-address=[::]:9090"]
