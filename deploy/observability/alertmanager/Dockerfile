# Alertmanager com o roteamento, os modelos das notificações e o script de
# partida embutidos (ADR-020: sem bind mounts; ADR-065). Os receptores de
# e-mail e de webhook são gerados na partida do container a partir das
# variáveis ALERTMANAGER_*: a imagem não contém endereços nem segredos.
FROM prom/alertmanager:v0.35.0
COPY alertmanager.yml /etc/alertmanager/alertmanager.yml
COPY templates/ /etc/alertmanager/templates/
COPY entrypoint.sh /etc/alertmanager/entrypoint.sh
# Falha o build se o roteamento, os modelos ou a configuração gerada forem
# inválidos: sem receptores e com todos eles (valores fictícios, descartados
# ao fim da verificação).
RUN /bin/sh /etc/alertmanager/entrypoint.sh check-config \
 && ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 \
    ALERTMANAGER_SMTP_USERNAME=alertas \
    ALERTMANAGER_SMTP_PASSWORD=verificacao-do-build \
    ALERTMANAGER_EMAIL_TO=plantao@exemplo.test \
    ALERTMANAGER_EMAIL_TO_CRITICAL=plantao@exemplo.test,gestao@exemplo.test \
    ALERTMANAGER_WEBHOOK_URL=https://alertas.exemplo.test/hook \
    ALERTMANAGER_WEBHOOK_BEARER_TOKEN=verificacao-do-build \
    /bin/sh /etc/alertmanager/entrypoint.sh check-config \
 && for t in subject text html; do \
      amtool template render --template.glob='/etc/alertmanager/templates/*.tmpl' \
        --template.text="{{ template \"recorvia.email.$t\" . }}" >/dev/null || exit 1; \
    done \
 && test ! -e /etc/alertmanager/runtime
# O script é chamado pelo interpretador: não depende do bit de execução do
# arquivo no checkout.
ENTRYPOINT ["/bin/sh", "/etc/alertmanager/entrypoint.sh"]
CMD []
EXPOSE 9093
