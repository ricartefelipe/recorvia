import { Tabs } from '@mantine/core';
import { Outlet, useLocation, useNavigate } from 'react-router';
import { PageHeader } from '../../components/ui';

const SECTIONS = [
  { value: 'planos', label: 'Planos e preços' },
  { value: 'produtos', label: 'Produtos' },
  { value: 'metricas', label: 'Métricas de consumo' },
  { value: 'cupons', label: 'Cupons' },
];

export function CatalogLayout() {
  const location = useLocation();
  const navigate = useNavigate();
  const current = location.pathname.split('/')[2] ?? 'planos';
  const isDetail = location.pathname.split('/').length > 3;
  return (
    <>
      {!isDetail && (
        <>
          <PageHeader
            title="Catálogo"
            description="Produtos, planos e versões de preço imutáveis. Mudar o preço cria uma nova versão; assinaturas existentes mantêm a versão contratada."
          />
          {/* Cada aba controla um painel existente (aria-controls válido); só o da seção atual tem conteúdo. */}
          <Tabs value={current} onChange={(v) => v && navigate(`/catalogo/${v}`)}>
            <Tabs.List mb="md">
              {SECTIONS.map((s) => (
                <Tabs.Tab key={s.value} value={s.value}>
                  {s.label}
                </Tabs.Tab>
              ))}
            </Tabs.List>
            {SECTIONS.map((s) => (
              <Tabs.Panel key={s.value} value={s.value}>
                {s.value === current && <Outlet />}
              </Tabs.Panel>
            ))}
          </Tabs>
        </>
      )}
      {isDetail && <Outlet />}
    </>
  );
}
