# Keycloak de produção do Recorvia (ADR-086): modo de produção (start
# --optimized) com PostgreSQL e o realm de produção, sem usuários de
# demonstração, embutido na imagem (ADR-020: sem bind mounts).
# Uso (contexto é a pasta deploy/producao/keycloak):
#   docker build -t recorvia-keycloak-producao:<versão> deploy/producao/keycloak
FROM quay.io/keycloak/keycloak:26.8.2 AS build
# Opções de build: fixadas na imagem e aplicadas por "kc.sh build".
ENV KC_DB=postgres \
    KC_HEALTH_ENABLED=true \
    KC_METRICS_ENABLED=true
RUN /opt/keycloak/bin/kc.sh build

FROM quay.io/keycloak/keycloak:26.8.2
COPY --from=build /opt/keycloak/ /opt/keycloak/
# Importado somente na primeira partida (o realm ainda não existe no banco);
# os marcadores ${...} são resolvidos a partir das variáveis de ambiente do
# container.
COPY --chown=1000:0 realm-producao.json /opt/keycloak/data/import/realm-recorvia.json
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
CMD ["start", "--optimized", "--import-realm"]
