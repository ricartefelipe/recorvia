import { Alert } from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';

/** Aviso do documento comercial (o mesmo texto do documento HTML): a fatura não substitui a nota fiscal (RF-06). */
export const FISCAL_NOTICE = 'Este documento é uma fatura comercial e não é documento fiscal. Não substitui a nota fiscal.';

/** Aviso fiscal exibido na página da fatura, no painel e no portal do cliente. */
export function FiscalNotice() {
  return (
    <Alert variant="light" icon={<IconInfoCircle />} p="xs" role="note" aria-label="Aviso fiscal">
      {FISCAL_NOTICE}
    </Alert>
  );
}
