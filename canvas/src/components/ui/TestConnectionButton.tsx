'use client';

import { useState, useCallback, useRef, useEffect } from 'react';
import type { TestConnectionState, SecretGroup } from '@/types/secrets';
import { validateSecret, ApiError } from '@/lib/api/secrets';

interface TestConnectionButtonProps {
  provider: SecretGroup;
  secretValue: string;
  onResult?: (valid: boolean) => void;
}

const LABELS: Record<TestConnectionState, string> = {
  idle: 'Test connection',
  testing: 'Testing\u2026',
  success: 'Connected \u2713',
  failure: 'Test failed',
};

const RESET_DELAYS: Record<string, number> = {
  success: 3000,
  failure: 5000,
};

/**
 * Optional test-connection button shown for supported services (GitHub,
 * Anthropic, OpenRouter). Hidden entirely for custom keys.
 *
 * States: idle → testing → success/failure → auto-reset to idle.
 */
export function TestConnectionButton({
  provider,
  secretValue,
  onResult,
}: TestConnectionButtonProps) {
  const [state, setState] = useState<TestConnectionState>('idle');
  const [errorDetail, setErrorDetail] = useState<string | null>(null);
  const resetTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Clean up timer on unmount
  useEffect(() => {
    return () => clearTimeout(resetTimerRef.current);
  }, []);

  const handleTest = useCallback(async () => {
    setState('testing');
    setErrorDetail(null);
    clearTimeout(resetTimerRef.current);
    try {
      const result = await validateSecret(provider, secretValue);
      const nextState = result.valid ? 'success' : 'failure';
      setState(nextState);
      if (!result.valid) {
        setErrorDetail(result.error ?? 'Could not verify key. Check it has the required permissions.');
      }
      onResult?.(result.valid);
      resetTimerRef.current = setTimeout(() => setState('idle'), RESET_DELAYS[nextState]!);
    } catch (err) {
      // Distinguish a real failure shape rather than always claiming a
      // timeout. A reachable server that answered with an HTTP status
      // (ApiError) did NOT time out — most commonly the validation route
      // is not available (404/501), which must not masquerade as
      // "service down". Only an actual thrown network/abort error is a
      // connectivity failure.
      setState('failure');
      if (err instanceof ApiError) {
        setErrorDetail(
          err.status === 404 || err.status === 501
            ? 'Key validation is not available for this service yet. The key was not tested.'
            : `Could not verify key (server returned ${err.status}). Saving is unaffected.`,
        );
      } else {
        setErrorDetail('Could not reach the validation service. Check your connection and try again.');
      }
      onResult?.(false);
      resetTimerRef.current = setTimeout(() => setState('idle'), RESET_DELAYS.failure);
    }
  }, [provider, secretValue, onResult]);

  return (
    <div className="test-connection">
      <button
        type="button"
        onClick={handleTest}
        disabled={state === 'testing' || !secretValue}
        className={`test-connection__btn test-connection__btn--${state}`}
      >
        {state === 'testing' && <Spinner />}
        {LABELS[state]}
      </button>
      {errorDetail && state === 'failure' && (
        <p className="test-connection__error" role="alert">
          {errorDetail}
        </p>
      )}
    </div>
  );
}

function Spinner() {
  return (
    <svg className="spinner" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
      <path d="M12 2v4M12 18v4M4.93 4.93l2.83 2.83M16.24 16.24l2.83 2.83M2 12h4M18 12h4M4.93 19.07l2.83-2.83M16.24 7.76l2.83-2.83" />
    </svg>
  );
}
