# Build context: deploy/
FROM grafana/grafana:12.1.1
COPY railway/grafana/datasource.yml /etc/grafana/provisioning/datasources/prometheus.yml
COPY grafana/provisioning/dashboards/dashboards.yml /etc/grafana/provisioning/dashboards/dashboards.yml
COPY grafana/dashboards/ /var/lib/grafana/dashboards/
ENV GF_AUTH_ANONYMOUS_ENABLED=true \
    GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer \
    GF_USERS_ALLOW_SIGN_UP=false \
    GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/gpu-fleet.json \
    GF_SERVER_HTTP_ADDR=:: \
    GF_ANALYTICS_REPORTING_ENABLED=false
