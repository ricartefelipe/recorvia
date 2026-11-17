import { Text, TextInput, type TextInputProps } from '@mantine/core';

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
