import type { SecretStatus } from '@/types/secrets';

interface StatusBadgeProps {
  status: SecretStatus;
}

const CONFIG: Record<SecretStatus, { icon: string; label: string; className: string }> = {
  verified:   { icon: '✓', label: 'Connection status: verified',   className: 'status-badge--valid' },
  invalid:    { icon: '✗', label: 'Connection status: invalid',    className: 'status-badge--invalid' },
  unverified: { icon: '○', label: 'Connection status: unverified', className: 'status-badge--unverified' },
};

/**
 * Status indicator for a secret key.
 * Per spec: always icon + color (never color-only) for colour-blind users.
 */
export function StatusBadge({ status }: StatusBadgeProps) {
  const { icon, label, className } = CONFIG[status];
  return (
    <span
      role="status"
      aria-label={label}
      className={`status-badge ${className}`}
    >
      {icon}
    </span>
  );
}
