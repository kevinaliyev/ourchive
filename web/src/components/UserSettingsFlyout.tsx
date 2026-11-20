/**
 * UserSettingsFlyout — Top-right user settings popover containing account identity,
 * theme switcher, animated gradient button toggle, transfer mode readout, and sign-out.
 */
import { useEffect, useRef, useState } from "react";
import { ChevronDown, LogOut, Radio, Settings } from "lucide-react";
import { PublicNetworkConfig, User } from "../api";

interface UserSettingsFlyoutProps {
  user: User;
  scheme: "dark" | "light";
  onChangeScheme: (next: "dark" | "light") => void;
  btnMotion: boolean;
  onChangeBtnMotion: (next: boolean) => void;
  netConfig: PublicNetworkConfig | null;
  onLogout: () => Promise<void>;
}

export function UserSettingsFlyout({
  user,
  scheme,
  onChangeScheme,
  btnMotion,
  onChangeBtnMotion,
  netConfig,
  onLogout,
}: UserSettingsFlyoutProps) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocClick(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="user-flyout-wrap" ref={wrapRef}>
      <button
        type="button"
        className="btn"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Settings size={14} strokeWidth={1.6} />
        <span>{user.display_name}</span>
        <ChevronDown size={13} strokeWidth={1.6} />
      </button>

      {open && (
        <div className="user-flyout card" role="dialog" aria-label="User settings">
          <div className="user-flyout-head">
            <div>
              <strong>{user.display_name}</strong>
              <div className="inline-muted mono" style={{ fontSize: 12 }}>
                {user.username ? `@${user.username}` : user.email || "Member"}
              </div>
            </div>
            <span className="tag-chip mono">{user.role}</span>
          </div>

          <div className="user-flyout-section">
            <div className="cap-row">
              <div>
                <strong>Theme</strong>
                <small>Warm Paper palette</small>
              </div>
              <div className="segmented" role="group" aria-label="Theme preference">
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={scheme === "dark"}
                  onClick={() => onChangeScheme("dark")}
                >
                  Dark
                </button>
                <button
                  type="button"
                  className="seg-btn"
                  aria-pressed={scheme === "light"}
                  onClick={() => onChangeScheme("light")}
                >
                  Light
                </button>
              </div>
            </div>

            <label className="switch-row">
              <span>
                <strong>Animated primary buttons</strong>
                <small>Subtle iridescent gradient drift</small>
              </span>
              <input
                type="checkbox"
                checked={btnMotion}
                onChange={(e) => onChangeBtnMotion(e.target.checked)}
                {...({ switch: "" } as Record<string, string>)}
              />
            </label>

            <div className="user-flyout-meta">
              <Radio size={13} strokeWidth={1.6} />
              <span>
                {netConfig?.p2p_enabled
                  ? netConfig.allow_relay_fallback
                    ? "WebRTC P2P (with Hub fallback)"
                    : "WebRTC P2P direct only"
                  : "Hub relay streaming"}
              </span>
            </div>
          </div>

          <div className="user-flyout-foot">
            <button
              type="button"
              className="btn btn-danger"
              style={{ width: "100%" }}
              onClick={() => {
                setOpen(false);
                void onLogout();
              }}
            >
              <LogOut size={14} strokeWidth={1.6} />
              <span>Sign out</span>
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
