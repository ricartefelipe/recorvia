import { useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';

/**
 * Envolve uma `<Table>` do Mantine e a torna responsiva (ADR-090).
 *
 * - `cards` (padrão): abaixo do breakpoint `sm` (48em), cada linha vira um
 *   cartão com pares rótulo e valor. Os rótulos vêm dos cabeçalhos da tabela
 *   (texto visível do `<th>`) e podem ser trocados com `data-label` na célula.
 *   A semântica de tabela é mantida com papéis ARIA explícitos, porque alguns
 *   navegadores descartam a semântica de tabelas exibidas como bloco.
 * - `scroll`: mantém a tabela e a rolagem horizontal. Quando há rolagem, a
 *   região recebe foco pelo teclado, nome acessível e uma dica visível.
 *
 * Acima do breakpoint, `minWidth` garante a largura mínima da tabela (com
 * rolagem horizontal se a área for mais estreita), como o
 * `Table.ScrollContainer` do Mantine.
 */
export function ResponsiveTable({
  children,
  minWidth,
  mode = 'cards',
  label,
}: {
  children: ReactNode;
  minWidth?: number;
  mode?: 'cards' | 'scroll';
  /** Nome da região de rolagem; por padrão, o aria-label da tabela. */
  label?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [overflowing, setOverflowing] = useState(false);
  const [regionLabel, setRegionLabel] = useState(label);

  useLayoutEffect(() => {
    const root = ref.current;
    if (!root) return;
    const table = root.querySelector('table');
    if (!table) return;
    const apply = () => labelTable(table);
    apply();
    setRegionLabel(label ?? table.getAttribute('aria-label') ?? undefined);
    if (typeof MutationObserver === 'undefined') return;
    const observer = new MutationObserver(apply);
    observer.observe(table, { childList: true, subtree: true });
    return () => observer.disconnect();
  }, [label]);

  useLayoutEffect(() => {
    const root = ref.current;
    if (!root || typeof ResizeObserver === 'undefined') return;
    const measure = () => setOverflowing(root.scrollWidth > root.clientWidth + 1);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(root);
    const table = root.querySelector('table');
    if (table) observer.observe(table);
    return () => observer.disconnect();
  }, []);

  const style = minWidth ? ({ '--rt-min-width': `${minWidth}px` } as CSSProperties) : undefined;
  return (
    <div className="rt" data-mode={mode} data-overflowing={overflowing || undefined}>
      <div
        ref={ref}
        className="rt-scroll"
        style={style}
        // Região rolável precisa ser alcançável pelo teclado (WCAG 2.1.1).
        tabIndex={overflowing ? 0 : undefined}
        role={overflowing ? 'region' : undefined}
        aria-label={overflowing && regionLabel ? `${regionLabel} (role para os lados para ver todas as colunas)` : undefined}
      >
        {children}
      </div>
      {overflowing && (
        <p className="rt-hint" aria-hidden>
          Role para os lados para ver todas as colunas.
        </p>
      )}
    </div>
  );
}

/** Texto visível de um cabeçalho, sem o conteúdo apenas para leitores de tela. */
function headerText(th: Element): string {
  const clone = th.cloneNode(true) as Element;
  clone.querySelectorAll('.visually-hidden, [aria-hidden="true"]').forEach((el) => el.remove());
  return (clone.textContent ?? '').replace(/\s+/g, ' ').trim();
}

/** Aplica papéis ARIA e os rótulos usados no modo de cartões. */
function labelTable(table: HTMLTableElement) {
  setAttr(table, 'role', 'table');
  const headRows = table.tHead ? Array.from(table.tHead.rows) : [];
  const headers: string[] = [];
  const lastHead = headRows[headRows.length - 1];
  if (lastHead) {
    for (const cell of Array.from(lastHead.cells)) {
      const text = headerText(cell);
      for (let i = 0; i < Math.max(1, cell.colSpan); i++) headers.push(text);
    }
  }
  for (const group of [table.tHead, ...Array.from(table.tBodies), table.tFoot]) {
    if (!group) continue;
    setAttr(group, 'role', 'rowgroup');
    for (const row of Array.from(group.rows)) {
      setAttr(row, 'role', 'row');
      let column = 0;
      for (const cell of Array.from(row.cells)) {
        const isHead = group === table.tHead;
        if (cell.tagName === 'TH') {
          setAttr(cell, 'role', isHead ? 'columnheader' : 'rowheader');
        } else {
          setAttr(cell, 'role', 'cell');
        }
        if (!isHead) {
          const span = Math.max(1, cell.colSpan);
          const explicit = cell.getAttribute('data-label');
          const text = explicit ?? (span === 1 ? (headers[column] ?? '') : '');
          setAttr(cell, 'data-rt-label', text);
          column += span;
        }
      }
    }
  }
}

function setAttr(el: Element, name: string, value: string) {
  if (el.getAttribute(name) !== value) el.setAttribute(name, value);
}
