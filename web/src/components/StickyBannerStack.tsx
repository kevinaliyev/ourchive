/**
 * StickyBannerStack — Pinned top-of-canvas notification & Admin broadcast stack.
 * Supports four distinct visual tiers (`broadcast`, `notice`, `warning`, `error`),
 * optional action buttons, and per-message dismissal.
 */
import { AlertOctagon, AlertTriangle, Bell, Megaphone, X } from "lucide-react";
import { AnnouncementKind } from "../api";

export interface StickyMessage {
  id: string;
  kind: AnnouncementKind;
  title?: string;
  body: string;
  dismissible: boolean;
  actionLabel?: string;
  onAction?: () => void;
}

interface StickyBannerStackProps {
  messages: StickyMessage[];
  onDismiss: (id: string) => void;
}

function kindMeta(kind: AnnouncementKind) {
  switch (kind) {
    case "error":
      return { icon: AlertOctagon, label: "Error" };
    case "warning":
      return { icon: AlertTriangle, label: "Notice" };
    case "notice":
      return { icon: Bell, label: "Notification" };
    default:
      return { icon: Megaphone, label: "Broadcast" };
  }
}

export function StickyBannerStack({ messages, onDismiss }: StickyBannerStackProps) {
  if (messages.length === 0) return null;

  return (
    <div className="sticky-banner-stack" role="region" aria-label="Announcements and notifications">
      <div className="sticky-banner-inner">
        {messages.map((m) => {
          const { icon: Icon, label } = kindMeta(m.kind);
          return (
            <div key={m.id} className={`sticky-banner sticky-${m.kind}`} role={m.kind === "error" ? "alert" : "status"}>
              <div className="sticky-banner-main">
                <Icon size={15} strokeWidth={1.7} className="sticky-banner-icon" />
                <span className="sticky-banner-badge mono">{label}</span>
                <div className="sticky-banner-copy">
                  {m.title && <strong className="sticky-banner-title">{m.title}</strong>}
                  <span>{m.body}</span>
                </div>
              </div>
              <div className="sticky-banner-actions">
                {m.actionLabel && m.onAction && (
                  <button type="button" className="btn btn-sm" onClick={m.onAction}>
                    {m.actionLabel}
                  </button>
                )}
                {m.dismissible && (
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label="Dismiss message"
                    onClick={() => onDismiss(m.id)}
                  >
                    <X size={14} strokeWidth={1.6} />
                  </button>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
