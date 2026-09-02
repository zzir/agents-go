import { useEffect } from 'react';
import { toast } from '@/lib/toast';

// A list that failed to load says so as a toast, like every other panel —
// once per failure, not once per render.
export function useLoadError(error: string | null, what: string): void {
  useEffect(() => {
    if (error) toast.error(`Failed to load ${what}: ${error}`);
  }, [error, what]);
}
