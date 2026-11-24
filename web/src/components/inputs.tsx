import { Text, TextInput, type ComboboxItem, type OptionsFilter, type TextInputProps } from '@mantine/core';

/**
 * Valor em reais digitado como texto ("1.234,56"). A conversão para centavos
 * é feita com aritmética de texto/BigInt na validação, nunca com float.
 */
export function MoneyInput({ description, ...props }: TextInputProps) {
  return (
    <TextInput
      inputMode="decimal"
      autoComplete="off"
      placeholder="0,00"
      leftSection={
        <Text size="sm" c="dimmed" aria-hidden>
          R$
        </Text>
      }
      description={description ?? 'Use vírgula para os centavos, por exemplo 1.234,56.'}
      {...props}
    />
  );
}

const fold = (text: string) =>
  text
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase();

/**
 * Filtro de Select que ignora acentos e também procura no valor (por exemplo,
 * "sao paulo" encontra "Horário de Brasília" pelo código America/Sao_Paulo).
 */
export const accentInsensitiveFilter: OptionsFilter = ({ options, search }) => {
  const term = fold(search.trim()).replace(/\s+/g, ' ');
  if (!term) return options;
  return (options as ComboboxItem[]).filter(
    (o) => fold(o.label).includes(term) || fold(o.value).replace(/[_/]/g, ' ').includes(term),
  );
};
