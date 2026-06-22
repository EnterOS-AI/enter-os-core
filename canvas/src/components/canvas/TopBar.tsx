import { SettingsButton } from '@/components/settings/SettingsButton';
import { settingsGearRef } from '@/components/settings/SettingsPanel';

interface TopBarProps {
  canvasName?: string;
}

/**
 * Canvas top bar component.
 *
 * Per spec §1.1, the gear icon sits in the right cluster:
 *   [Logo]  [Canvas Name ▾]    [+ New Agent]  [⚙]  [🔔]  [Avatar]
 *
 * This is a minimal scaffold — the real TopBar in the canvas repo will
 * already have the other elements. The integration point is adding
 * <SettingsButton /> into the right cluster.
 */
export function TopBar({ canvasName = 'Canvas' }: TopBarProps) {
  return (
    <header className="top-bar">
      <div className="top-bar__left">
        <span className="top-bar__logo" aria-hidden="true">☁</span>
        <span className="top-bar__name">{canvasName}</span>
      </div>
      <div className="top-bar__right">
        <button className="top-bar__btn">+ New Agent</button>
        {/* === INTEGRATION POINT: Settings gear icon === */}
        <SettingsButton ref={settingsGearRef} />
        {/* Bell and Avatar would go here */}
      </div>
    </header>
  );
}
