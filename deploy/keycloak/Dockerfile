# Keycloak local com o realm de demonstração embutido (sem bind mount).
FROM quay.io/keycloak/keycloak:26.8.2
COPY --chown=1000:0 realm-recorvia.json /opt/keycloak/data/import/realm-recorvia.json
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
CMD ["start-dev", "--import-realm"]
