import {
  ActionIcon,
  Alert,
  Button,
  Code,
  CopyButton,
  Group,
  Modal,
  SegmentedControl,
  Stack,
  Text,
  TextInput,
  Title,
  Tooltip,
  type SegmentedControlProps,
  type TextInputProps,
} from '@mantine/core';
import { modals } from '@mantine/modals';
import { IconCheck, IconChevronLeft, IconChevronRight, IconCopy, IconInfoCircle, IconKey } from '@tabler/icons-react';
import { useEffect, useId, useState, type ReactNode } from 'react';
import { setAnnouncer } from '../lib/notify';

export function PageHeader({
  title,
  description,
  actions,
  badge,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  badge?: ReactNode;
}) {
  return (
    <Group justify="space-between" align="flex-start" mb="lg" wrap="wrap" gap="sm">
      <Stack gap={4}>
        <Group gap="sm">
          <Title order={1} size="h2">
            {title}
          </Title>
          {badge}
        </Group>
        {description && (
          <Text c="dimmed" size="sm" maw={720}>
            {description}
          </Text>
        )}
      </Stack>
      {actions && <Group gap="xs">{actions}</Group>}
    </Group>
  );
}

/** Ícone de ajuda com explicação acessível por teclado. */
export function HelpTip({ label, text }: { label: string; text: string }) {
  return (
    <Tooltip label={text} multiline w={320} withArrow events={{ hover: true, focus: true, touch: true }}>
      <ActionIcon variant="subtle" color="gray" size="sm" aria-label={`Como calculamos: ${label}`}>
        <IconInfoCircle size={16} />
      </ActionIcon>
    </Tooltip>
  );
}

export function CopyValueButton({ value, label = 'Copiar' }: { value: string; label?: string }) {
  return (
    <CopyButton value={value} timeout={2000}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copiado' : label} withArrow>
          <ActionIcon variant="light" color={copied ? 'teal' : 'blue'} onClick={copy} aria-label={copied ? 'Copiado' : label}>
            {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
          </ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  );
}

/** Campo somente leitura com botão de copiar. */
export function CopyField({ value, label, ...props }: { value: string; label: string } & Omit<TextInputProps, 'value'>) {
  return (
    <TextInput
      label={label}
      value={value}
      readOnly
      onFocus={(e) => e.currentTarget.select()}
      rightSection={<CopyValueButton value={value} label={`Copiar ${label.toLowerCase()}`} />}
      rightSectionWidth={44}
      {...props}
    />
  );
}

/**
 * Exibe um segredo ou link que a API mostra uma única vez. Não pode ser
 * fechado com clique fora para evitar perda acidental.
 */
export function SecretRevealModal({
  opened,
  title,
  description,
  value,
  valueLabel,
  onClose,
}: {
  opened: boolean;
  title: string;
  description?: ReactNode;
  value: string | null;
  valueLabel: string;
  onClose: () => void;
}) {
  return (
    <Modal opened={opened && !!value} onClose={onClose} title={title} size="lg" closeOnClickOutside={false}>
      <Stack>
        <Alert color="orange" icon={<IconKey />} title="Exibido uma única vez">
          Copie e guarde agora em local seguro. Este valor não será exibido novamente.
        </Alert>
        {description && <Text size="sm">{description}</Text>}
        {value && <CopyField label={valueLabel} value={value} styles={{ input: { fontFamily: 'monospace' } }} />}
        <Group justify="flex-end">
          <Button onClick={onClose}>Já copiei, fechar</Button>
        </Group>
      </Stack>
    </Modal>
  );
}

/** Confirmação em modal; resolve true somente ao confirmar. */
export function confirmAction(options: {
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
}): Promise<boolean> {
  return new Promise((resolve) => {
    let settled = false;
    const settle = (value: boolean) => {
      if (!settled) {
        settled = true;
        resolve(value);
      }
    };
    modals.openConfirmModal({
      title: options.title,
      centered: true,
      children: typeof options.message === 'string' ? <Text size="sm">{options.message}</Text> : options.message,
      labels: { confirm: options.confirmLabel ?? 'Confirmar', cancel: 'Cancelar' },
      confirmProps: { color: options.danger ? 'red' : 'blue' },
      onConfirm: () => settle(true),
      onCancel: () => settle(false),
      onClose: () => settle(false),
    });
  });
}

/** Aviso de alteração concorrente (HTTP 412) com recarga dos dados. */
export function openConcurrencyModal(onReload: () => void): void {
  const id = modals.open({
    title: 'Alteração concorrente',
    centered: true,
    children: (
      <Stack>
        <Text size="sm">
          Este registro foi alterado por outra pessoa depois que você o abriu. Recarregue os dados para ver a versão atual
          e refaça a alteração, se ainda for necessária.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={() => modals.close(id)}>
            Fechar
          </Button>
          <Button
            onClick={() => {
              modals.close(id);
              onReload();
            }}
          >
            Recarregar dados
          </Button>
        </Group>
      </Stack>
    ),
  });
}

export function CursorPager({
  page,
  hasPrevious,
  hasNext,
  onPrevious,
  onNext,
  loading,
}: {
  page: number;
  hasPrevious: boolean;
  hasNext: boolean;
  onPrevious: () => void;
  onNext: () => void;
  loading?: boolean;
}) {
  if (!hasPrevious && !hasNext) return null;
  return (
    <Group justify="space-between" mt="md" component="nav" aria-label="Paginação">
      <Text size="sm" c="dimmed">
        Página {page}
      </Text>
      <Group gap="xs">
        <Button
          variant="default"
          size="xs"
          leftSection={<IconChevronLeft size={14} />}
          disabled={!hasPrevious || loading}
          onClick={onPrevious}
        >
          Anterior
        </Button>
        <Button
          variant="default"
          size="xs"
          rightSection={<IconChevronRight size={14} />}
          disabled={!hasNext || loading}
          onClick={onNext}
        >
          Próxima
        </Button>
      </Group>
    </Group>
  );
}

/** Região aria-live que repete as notificações para leitores de tela. */
export function LiveRegion() {
  const [polite, setPolite] = useState('');
  const [assertive, setAssertive] = useState('');
  useEffect(() => {
    const timers: ReturnType<typeof setTimeout>[] = [];
    setAnnouncer((message, politeness) => {
      const set = politeness === 'assertive' ? setAssertive : setPolite;
      set('');
      timers.push(setTimeout(() => set(message), 60));
    });
    return () => {
      setAnnouncer(undefined);
      timers.forEach(clearTimeout);
    };
  }, []);
  return (
    <div className="visually-hidden">
      <div aria-live="polite" aria-atomic="true" data-testid="live-polite">
        {polite}
      </div>
      <div aria-live="assertive" aria-atomic="true" data-testid="live-assertive">
        {assertive}
      </div>
    </div>
  );
}

export function KeyValue({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
      <Text size="sm" component="div">
        {children}
      </Text>
    </Stack>
  );
}

export function InlineCode({ children }: { children: ReactNode }) {
  return <Code>{children}</Code>;
}

/** SegmentedControl com rótulo visível associado (grupo de opções acessível). */
export function LabeledSegmented({ label, ...props }: { label: string } & SegmentedControlProps) {
  const id = useId();
  return (
    <Stack gap={4}>
      <Text size="sm" fw={500} id={id}>
        {label}
      </Text>
      <SegmentedControl aria-labelledby={id} {...props} />
    </Stack>
  );
}
