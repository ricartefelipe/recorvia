-- Chave de negócio da criação de assinatura e posse da execução de uma
-- Idempotency-Key (RF-09, ADR-075).
--
-- A criação de assinatura é operação financeira de criação: a repetição
-- duplicaria a assinatura e o faturamento. O Idempotency-Key protege a
-- repetição pela resposta registrada; a chave de negócio derivada dele (64
-- caracteres, hex(SHA-256(cliente|operação|chave))) é gravada na assinatura
-- com a impressão dos campos de negócio do pedido, para que uma nova
-- tentativa depois de um processamento interrompido (lease expirado antes do
-- registro da resposta) ou da validade do registro HTTP devolva a assinatura
-- já criada em vez de criar outra, e que a mesma chave com outro conteúdo
-- seja recusada. Assinaturas criadas sem a chave (seed, testes, versões
-- anteriores) ficam com NULL, que não conflita.
--
-- lease_token identifica a execução que detém a chave em processamento: o
-- registro do resultado e a liberação exigem o mesmo token, de modo que uma
-- execução antiga, cujo lease foi assumido por outra, não sobrescreva o
-- resultado da nova. Chaves em processamento gravadas antes desta migration
-- ficam com NULL e só são concluídas depois de assumidas (lease expirado).

-- +goose Up

ALTER TABLE subscriptions ADD COLUMN request_key text CHECK (request_key IS NULL OR length(request_key) <= 300);
ALTER TABLE subscriptions ADD COLUMN request_fingerprint bytea CHECK (request_fingerprint IS NULL OR length(request_fingerprint) = 32);
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_request_key UNIQUE (tenant_id, request_key);
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_request_fingerprint
  CHECK ((request_key IS NULL) = (request_fingerprint IS NULL));

ALTER TABLE idempotency_keys ADD COLUMN lease_token uuid;

-- +goose Down
ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS lease_token;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_request_fingerprint;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_request_key;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS request_fingerprint;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS request_key;
