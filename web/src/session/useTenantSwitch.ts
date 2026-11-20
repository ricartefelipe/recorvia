import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useNavigate } from 'react-router';
import { handleMutationError } from '../lib/errors';
import { notifySuccess } from '../lib/notify';
import { performTenantSwitch } from './sessionApi';

/** Troca de organização: PUT, limpeza do cache, recarga da sessão e dashboard. */
export function useTenantSwitch() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [switching, setSwitching] = useState<string | null>(null);
  const switchTo = async (tenantId: string) => {
    setSwitching(tenantId);
    try {
      const session = await performTenantSwitch(queryClient, tenantId);
      notifySuccess(`Organização ativa: ${session.active_tenant?.name ?? ''}`.trim(), 'Organização alterada');
      navigate('/', { replace: true });
    } catch (error) {
      handleMutationError(error);
    } finally {
      setSwitching(null);
    }
  };
  return { switchTo, switching };
}
