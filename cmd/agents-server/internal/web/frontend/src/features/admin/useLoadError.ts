import { useEffect } from 'react';
import { toast } from '@/lib/toast';

// Toasts a list's load failure once per failure, not once per render.
export function useLoadError(error: string | null, what: string): void {
  useEffect(() => {
    if (error) toast.error(`Failed to load ${what}: ${error}`);
  }, [error, what]);
}
