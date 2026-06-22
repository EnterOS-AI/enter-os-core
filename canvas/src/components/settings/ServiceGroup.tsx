import type { Secret, SecretGroup, ServiceConfig } from '@/types/secrets';
import { SecretRow } from './SecretRow';

interface ServiceGroupProps {
  group: SecretGroup;
  service: ServiceConfig;
  secrets: Secret[];
  workspaceId: string;
}

/**
 * Collapsible group of secret rows under a service header.
 *
 * Per spec §3.1:
 *   ── GitHub ────────────────────────── 1 key ──
 *   GITHUB_TOKEN
 *   ghp-••••••••••••••xK9f  [👁] [✓] [⎘] [✏] [🗑]
 */
export function ServiceGroup({
  group,
  service,
  secrets,
  workspaceId,
}: ServiceGroupProps) {
  const countLabel = secrets.length === 1 ? '1 key' : `${secrets.length} keys`;

  return (
    <div className="service-group" role="group" aria-label={`${service.label} keys`}>
      <div className="service-group__header">
        <ServiceIcon name={service.icon} />
        <span className="service-group__label">{service.label}</span>
        <span className="service-group__count">{countLabel}</span>
      </div>
      <div className="service-group__rows">
        {secrets.map((secret) => (
          <SecretRow
            key={secret.name}
            secret={secret}
            workspaceId={workspaceId}
          />
        ))}
      </div>
    </div>
  );
}

function ServiceIcon({ name }: { name: string }) {
  // Placeholder — real implementation would use SVG imports or an icon component
  const icons: Record<string, string> = {
    github: '🐙',
    anthropic: '🤖',
    openrouter: '🔀',
    key: '🔑',
  };
  return (
    <span className="service-group__icon" aria-hidden="true">
      {icons[name] ?? '🔑'}
    </span>
  );
}
