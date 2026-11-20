# Grafana com fontes de dados e dashboards provisionados (ADR-020: sem bind
# mounts). Acesso anônimo somente leitura no ambiente local; a senha do
# administrador vem do compose (GRAFANA_ADMIN_PASSWORD).
FROM grafana/grafana:13.3.1
COPY provisioning/ /etc/grafana/provisioning/
COPY dashboards/ /etc/grafana/dashboards/recorvia/
ENV GF_ANALYTICS_REPORTING_ENABLED=false \
    GF_ANALYTICS_CHECK_FOR_UPDATES=false \
    GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false \
    GF_NEWS_NEWS_FEED_ENABLED=false \
    GF_USERS_ALLOW_SIGN_UP=false \
    GF_AUTH_ANONYMOUS_ENABLED=true \
    GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer \
    GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/etc/grafana/dashboards/recorvia/recorvia-visao-geral.json
EXPOSE 3000
