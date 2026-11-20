import { Alert, Button, Group, Loader, Modal, Stack, Text } from '@mantine/core';
import { IconDownload, IconPrinter } from '@tabler/icons-react';
import { useRef } from 'react';
import { describeError } from '../lib/errors';

/**
 * Exibe o documento HTML da fatura num iframe isolado (sem scripts). Fora do
 * iframe ficam as ações de imprimir e baixar.
 */
export function DocumentViewer({
  opened,
  onClose,
  title,
  html,
  loading,
  error,
  filename,
}: {
  opened: boolean;
  onClose: () => void;
  title: string;
  html: string | null;
  loading?: boolean;
  error?: unknown;
  filename: string;
}) {
  const frame = useRef<HTMLIFrameElement>(null);
  const download = () => {
    if (!html) return;
    const url = URL.createObjectURL(new Blob([html], { type: 'text/html;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return (
    <Modal opened={opened} onClose={onClose} title={title} size="xl">
      <Stack>
        <Text size="xs" c="dimmed">
          Documento comercial. Não é documento fiscal.
        </Text>
        {loading && (
          <Group justify="center" py="xl">
            <Loader />
          </Group>
        )}
        {!!error && (
          <Alert color="red" title="Não foi possível carregar o documento">
            {describeError(error).message}
          </Alert>
        )}
        {html && (
          <iframe
            ref={frame}
            title={title}
            srcDoc={html}
            sandbox="allow-same-origin allow-modals"
            style={{ width: '100%', height: '65vh', border: '1px solid var(--mantine-color-gray-3)', borderRadius: 8, background: 'white' }}
          />
        )}
        <Group justify="flex-end">
          <Button variant="default" leftSection={<IconDownload size={16} />} onClick={download} disabled={!html}>
            Baixar HTML
          </Button>
          <Button leftSection={<IconPrinter size={16} />} onClick={() => frame.current?.contentWindow?.print()} disabled={!html}>
            Imprimir
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
