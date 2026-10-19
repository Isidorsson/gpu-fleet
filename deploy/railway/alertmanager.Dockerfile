# Build context: deploy/
FROM prom/alertmanager:v0.34.1
COPY railway/alertmanager.yml /etc/alertmanager/alertmanager.yml
# The webhook token is a Railway secret: written to a file at start-up so it
# is never baked into the image.
ENTRYPOINT ["/bin/sh", "-c", "printf %s \"$REMEDIATION_WEBHOOK_TOKEN\" > /tmp/webhook_token && exec /bin/alertmanager --config.file=/etc/alertmanager/alertmanager.yml --storage.path=/tmp/alertmanager --web.listen-address=[::]:9093"]
