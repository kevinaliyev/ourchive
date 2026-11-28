/**
 * AdminDevConsole — Quake-style top-down translucent drop-down command console for admins.
 * Summoned with the `~` / `` ` `` key when enabled in Host Admin settings. Executes typed
 * Ourchive Hub API commands (users, dirs/mounts, banners, invites, audit tail, panel navigation)
 * with command history and tab completion.
 */
import { FormEvent, KeyboardEvent, useEffect, useRef, useState } from "react";
import { AnnouncementKind, api, formatMbps, HealthPayload, NodeView, User } from "../api";
import { hubHost, hubOrigin } from "../platform";

interface ConsoleLine {
  id: number;
  kind: "cmd" | "out" | "ok" | "err" | "warn";
  text: string;
}

interface AdminDevConsoleProps {
  open: boolean;
  onClose: () => void;
  currentUser: User;
  hub: HealthPayload | null;
  nodes: NodeView[];
  onNavigateTab: (tab: "explorer" | "converter" | "shares" | "contribution" | "admin") => void;
  onChangeScheme: (next: "dark" | "light") => void;
  onRefreshAnnouncements: () => Promise<void>;
  onRefreshNodes: () => Promise<void>;
}

const KNOWN_COMMANDS = [
  "help",
  "status",
  "users",
  "dirs",
  "nodes",
  "shares",
  "invites",
  "invite",
  "banner",
  "audit",
  "goto",
  "theme",
  "clear",
  "close",
];

function tokenize(input: string): string[] {
  const out: string[] = [];
  let cur = "";
  let quote: '"' | "'" | null = null;
  for (let i = 0; i < input.length; i++) {
    const ch = input[i];
    if (quote) {
      if (ch === quote) {
        quote = null;
      } else {
        cur += ch;
      }
    } else if (ch === '"' || ch === "'") {
      quote = ch;
    } else if (/\s/.test(ch)) {
      if (cur) {
        out.push(cur);
        cur = "";
      }
    } else {
      cur += ch;
    }
  }
  if (cur) out.push(cur);
  return out;
}

export function AdminDevConsole({
  open,
  onClose,
  currentUser,
  hub,
  nodes,
  onNavigateTab,
  onChangeScheme,
  onRefreshAnnouncements,
  onRefreshNodes,
}: AdminDevConsoleProps) {
  const [input, setInput] = useState("");
  const [history, setHistory] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1);
  const [lines, setLines] = useState<ConsoleLine[]>([
    {
      id: 1,
      kind: "warn",
      text: "Ourchive Admin Console — type 'help' for commands, '~' or Esc to hide.",
    },
  ]);
  const nextId = useRef(2);
  const inputRef = useRef<HTMLInputElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (open) {
      requestAnimationFrame(() => inputRef.current?.focus());
    } else {
      inputRef.current?.blur();
    }
  }, [open]);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [lines, open]);

  function push(kind: ConsoleLine["kind"], text: string) {
    setLines((prev) => [...prev.slice(-200), { id: nextId.current++, kind, text }]);
  }

  async function runCommand(raw: string) {
    const trimmed = raw.trim();
    if (!trimmed) return;

    push("cmd", `${currentUser.username || currentUser.display_name}@ourchive $ ${trimmed}`);
    setHistory((prev) => [trimmed, ...prev.slice(0, 49)]);
    setHistIdx(-1);

    const args = tokenize(trimmed);
    const cmd = args[0].toLowerCase();

    try {
      switch (cmd) {
        case "help":
        case "?": {
          push(
            "out",
            [
              "Available commands:",
              "  status                                    Hub version, online nodes, total traffic",
              "  users                                     List all users, roles & inviters",
              "  dirs | nodes                              List machines & shared folders/files",
              "  shares                                    List active share links",
              "  invites                                   List invite keys",
              "  invite create [--role member|group_admin] [--uses N] [--expires H] [note...]",
              "  banner <broadcast|notice|warning|error> [--title \"...\"] [--pinned] [--expires H] <message...>",
              "    Examples:",
              "      banner error --pinned --title \"Maintenance\" Restarting in 5m",
              "      banner warning GPU host offline for updates",
              "      banner notice New shared folder added",
              "  banner list | banner clear <id|all>       Inspect or remove sticky banners",
              "  audit [core|download|preview] [limit]     Tail recent audit log entries",
              "  goto <explorer|converter|shares|contribution|admin>  Jump to panel",
              "  theme <dark|light>                        Switch UI color scheme",
              "  clear | close                             Clear output or hide console",
            ].join("\n"),
          );
          break;
        }

        case "clear":
        case "cls":
          setLines([]);
          break;

        case "close":
        case "exit":
          onClose();
          break;

        case "status":
        case "whoami": {
          const online = nodes.filter((n) => n.status === "online").length;
          const up = nodes.reduce((a, n) => a + (n.traffic?.upload_bps ?? 0), 0);
          const down = nodes.reduce((a, n) => a + (n.traffic?.download_bps ?? 0), 0);
          push(
            "ok",
            `Hub: ${hub ? `online (${hub.version}, share=${hub.share_host})` : "offline"} | User: ${currentUser.display_name} (${currentUser.role}) | Nodes: ${online}/${nodes.length} online | Traffic: ↑${formatMbps(up)} ↓${formatMbps(down)}`,
          );
          break;
        }

        case "users": {
          const res = await api.listUsers();
          if (res.users.length === 0) {
            push("out", "No users found.");
          } else {
            const out = res.users
              .map(
                (u) =>
                  `${u.display_name.padEnd(16)} ${(u.username ? "@" + u.username : u.email || "-").padEnd(26)} ${u.role.padEnd(12)} inv=${u.invited_by_name || "root"}${u.can_invite ? " [trusted]" : ""}`,
              )
              .join("\n");
            push("out", out);
          }
          break;
        }

        case "dirs":
        case "mounts":
        case "nodes": {
          await onRefreshNodes();
          if (nodes.length === 0) {
            push("out", "No machines enrolled.");
          } else {
            const rows: string[] = [];
            for (const n of nodes) {
              rows.push(`[${n.status.toUpperCase()}] ${n.display_name} (${n.os}/${n.arch}, owner=${n.owner_name})`);
              if (n.mounts.length === 0) {
                rows.push("    (no shared folders)");
              } else {
                for (const m of n.mounts) {
                  rows.push(
                    `    - ${m.name} [${m.kind || "folder"}] ${m.enabled ? "on" : "off"} -> ${m.local_path}`,
                  );
                }
              }
            }
            push("out", rows.join("\n"));
          }
          break;
        }

        case "shares": {
          const res = await api.listShares();
          if (res.shares.length === 0) {
            push("out", "No active share links.");
          } else {
            push(
              "out",
              res.shares
                .map((s) => `${s.slug.padEnd(16)} views=${s.view_count}  ${s.title} -> ${s.url}`)
                .join("\n"),
            );
          }
          break;
        }

        case "invites": {
          const res = await api.listInvites();
          if (res.invites.length === 0) {
            push("out", "No invite keys.");
          } else {
            push(
              "out",
              res.invites
                .map(
                  (k) =>
                    `${k.code_prefix}…  role=${k.role.padEnd(11)} uses=${k.use_count}/${k.max_uses}  by=${k.creator_name}  ${k.revoked ? "[REVOKED]" : k.expired ? "[EXPIRED]" : "[ACTIVE]"} ${k.note}`,
                )
                .join("\n"),
            );
          }
          break;
        }

        case "invite": {
          if (args[1]?.toLowerCase() !== "create") {
            push("warn", "Usage: invite create [--role member|group_admin|host_admin] [--uses N] [--expires H] [note...]");
            break;
          }
          let role = "member";
          let maxUses = 1;
          let expiresInHours = 72;
          const noteParts: string[] = [];
          for (let i = 2; i < args.length; i++) {
            const tok = args[i];
            if ((tok === "--role" || tok === "-r") && args[i + 1]) {
              role = args[++i];
            } else if ((tok === "--uses" || tok === "-u") && args[i + 1]) {
              maxUses = Math.max(1, Number(args[++i]) || 1);
            } else if ((tok === "--expires" || tok === "-e") && args[i + 1]) {
              expiresInHours = Math.max(0, Number(args[++i]) || 0);
            } else if ((tok === "--note" || tok === "-n") && args[i + 1]) {
              noteParts.push(args[++i]);
            } else if (tok.startsWith("-")) {
              push("err", `Unknown flag '${tok}'. Use --role, --uses, --expires, or --note.`);
              return;
            } else if (i === 2 && (tok === "member" || tok === "group_admin" || tok === "host_admin")) {
              role = tok;
            } else if ((i === 2 || i === 3) && /^\d+$/.test(tok)) {
              maxUses = Math.max(1, Number(tok));
            } else {
              noteParts.push(tok);
            }
          }
          const res = await api.createInvite({
            role,
            max_uses: maxUses,
            expires_in_hours: expiresInHours,
            note: noteParts.join(" "),
          });
          const link = `${hubOrigin()}/?invite=${encodeURIComponent(res.invite.code || "")}`;
          push("ok", `Created [${role}, uses=${maxUses}] invite key: ${res.invite.code}\nLink: ${link}`);
          break;
        }

        case "banner":
        case "broadcast": {
          const sub = args[1]?.toLowerCase();
          if (!sub || sub === "help" || sub === "--help" || sub === "-h") {
            push(
              "warn",
              [
                "Usage: banner <broadcast|notice|warning|error> [--title \"...\"] [--pinned] [--expires H] <message...>",
                "  Aliases: warn -> warning, err/alert -> error, info/notification -> notice",
                "  Subcommands: banner list | banner clear <id-prefix|all>",
                "  Example: banner error --pinned --title \"Restart\" Hub restarting in 5m",
              ].join("\n"),
            );
            break;
          }
          if (sub === "list") {
            const res = await api.listAnnouncements(true);
            if (res.announcements.length === 0) {
              push("out", "No active banners.");
            } else {
              push(
                "out",
                res.announcements
                  .map(
                    (a, idx) =>
                      `#${String(idx + 1).padEnd(2)} ${a.id.slice(0, 8)}  [${a.kind}]  ${a.dismissible ? "dismissible" : "pinned"}  ${a.title ? a.title + ": " : ""}${a.body}`,
                  )
                  .join("\n"),
              );
            }
            break;
          }
          if (sub === "clear") {
            const rawTargets = args.slice(2);
            if (rawTargets.length === 0) {
              push("warn", "Usage: banner clear <1,2,3 | 1-4 | id-prefix | all>");
              break;
            }
            const res = await api.listAnnouncements(true);
            const selected = new Map<string, (typeof res.announcements)[number]>();
            const tokens = rawTargets.flatMap((t) => t.split(",")).map((t) => t.trim()).filter(Boolean);
            for (const tok of tokens) {
              if (tok.toLowerCase() === "all") {
                for (const a of res.announcements) selected.set(a.id, a);
                continue;
              }
              const rangeMatch = tok.match(/^(\d+)-(\d+)$/);
              if (rangeMatch) {
                const start = Math.max(1, Number(rangeMatch[1]));
                const end = Math.min(res.announcements.length, Number(rangeMatch[2]));
                for (let n = start; n <= end; n++) {
                  const item = res.announcements[n - 1];
                  if (item) selected.set(item.id, item);
                }
                continue;
              }
              if (/^\d+$/.test(tok)) {
                const item = res.announcements[Number(tok) - 1];
                if (item) selected.set(item.id, item);
                continue;
              }
              for (const a of res.announcements) {
                if (a.id.startsWith(tok)) selected.set(a.id, a);
              }
            }
            if (selected.size === 0) {
              push("err", `No banners matched '${rawTargets.join(" ")}'. Run 'banner list' to see # numbers.`);
              break;
            }
            for (const m of selected.values()) {
              await api.deleteAnnouncement(m.id);
            }
            await onRefreshAnnouncements();
            push("ok", `Cleared ${selected.size} banner(s).`);
            break;
          }

          const kindAliases: Record<string, AnnouncementKind> = {
            broadcast: "broadcast",
            admin: "broadcast",
            announce: "broadcast",
            notice: "notice",
            notification: "notice",
            info: "notice",
            warning: "warning",
            warn: "warning",
            error: "error",
            err: "error",
            critical: "error",
            alert: "error",
          };

          let kind: AnnouncementKind = "broadcast";
          let startIdx = 1;
          if (kindAliases[sub]) {
            kind = kindAliases[sub];
            startIdx = 2;
          }

          let title = "";
          let dismissible = true;
          let expiresInHours = 0;
          const bodyParts: string[] = [];

          for (let i = startIdx; i < args.length; i++) {
            const tok = args[i];
            const lower = tok.toLowerCase();
            if (lower === "--pinned" || lower === "-p") {
              dismissible = false;
            } else if ((lower === "--title" || lower === "-t") && args[i + 1]) {
              title = args[++i];
            } else if ((lower === "--kind" || lower === "-k" || lower === "--type") && args[i + 1]) {
              const k = kindAliases[args[++i].toLowerCase()];
              if (!k) {
                push("err", "Invalid banner kind. Use: broadcast, notice, warning, or error.");
                return;
              }
              kind = k;
            } else if ((lower === "--expires" || lower === "-e") && args[i + 1]) {
              expiresInHours = Math.max(0, Number(args[++i]) || 0);
            } else if (lower.startsWith("--") && kindAliases[lower.slice(2)]) {
              kind = kindAliases[lower.slice(2)];
            } else if (lower.startsWith("-")) {
              push("err", `Unknown flag '${tok}'. Run 'banner help' for valid options.`);
              return;
            } else {
              bodyParts.push(tok);
            }
          }

          const body = bodyParts.join(" ").trim();
          if (!body) {
            push("err", "Banner message text is required. Example: banner error Hub restarting soon");
            break;
          }
          await api.createAnnouncement({
            title: title || undefined,
            kind,
            body,
            dismissible,
            expires_in_hours: expiresInHours,
          });
          await onRefreshAnnouncements();
          push("ok", `Published [${kind}] banner${dismissible ? "" : " (pinned)"}${title ? ` "${title}"` : ""}: ${body}`);
          break;
        }

        case "audit": {
          const cat = args[1] && ["core", "download", "preview"].includes(args[1]) ? args[1] : "all";
          const limit = args[2] && !Number.isNaN(Number(args[2])) ? Math.min(30, Math.max(1, Number(args[2]))) : 12;
          const res = await api.listAuditLogs({ category: cat, limit });
          if (res.entries.length === 0) {
            push("out", "No matching audit entries.");
          } else {
            push(
              "out",
              res.entries
                .map(
                  (e) =>
                    `${new Date(e.created_at).toLocaleTimeString()}  [${e.category}]  ${e.action.padEnd(26)}  ${e.actor_label}  ${e.target_id || ""}`,
                )
                .join("\n"),
            );
          }
          break;
        }

        case "goto":
        case "cd":
        case "panel": {
          const dest = (args[1] || "").toLowerCase();
          const map: Record<string, "explorer" | "converter" | "shares" | "contribution" | "admin"> = {
            explorer: "explorer",
            archive: "explorer",
            files: "explorer",
            converter: "converter",
            convert: "converter",
            shares: "shares",
            links: "shares",
            contribution: "contribution",
            nodes: "contribution",
            admin: "admin",
            people: "admin",
            audit: "admin",
          };
          const resolved = map[dest];
          if (!resolved) {
            push("err", "Unknown panel. Use: explorer, converter, shares, contribution, or admin.");
            break;
          }
          onNavigateTab(resolved);
          push("ok", `Switched to ${resolved} panel.`);
          break;
        }

        case "theme": {
          const next = args[1]?.toLowerCase();
          if (next !== "dark" && next !== "light") {
            push("warn", "Usage: theme <dark|light>");
            break;
          }
          onChangeScheme(next);
          push("ok", `Theme switched to ${next}.`);
          break;
        }

        default:
          push("err", `Unknown command '${cmd}'. Type 'help' for available commands.`);
      }
    } catch (err) {
      push("err", err instanceof Error ? err.message : "Command failed");
    }
  }

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const val = input;
    setInput("");
    void runCommand(val);
  }

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.code === "Backquote" || e.key === "`" || e.key === "~" || e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      inputRef.current?.blur();
      onClose();
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (history.length === 0) return;
      const next = Math.min(history.length - 1, histIdx + 1);
      setHistIdx(next);
      setInput(history[next]);
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (histIdx <= 0) {
        setHistIdx(-1);
        setInput("");
      } else {
        const next = histIdx - 1;
        setHistIdx(next);
        setInput(history[next]);
      }
      return;
    }
    if (e.key === "Tab") {
      e.preventDefault();
      const lower = input.trim().toLowerCase();
      if (!lower || lower.includes(" ")) return;
      const match = KNOWN_COMMANDS.find((c) => c.startsWith(lower));
      if (match) setInput(match + " ");
    }
  }

  return (
    <div
      className={`dev-console-overlay ${open ? "open" : ""}`}
      role="dialog"
      aria-label="Ourchive Admin Dev Console"
      aria-hidden={!open}
    >
      <div className="dev-console-head">
        <span className="dev-console-title mono">
          ourchive · {hubHost()}
        </span>
        <span className="dev-console-hint mono">~ / Esc to hide · Tab to autocomplete</span>
      </div>

      <div className="dev-console-output mono" ref={scrollRef}>
        {lines.map((l) => (
          <pre key={l.id} className={`dev-line dev-line-${l.kind}`}>
            {l.text}
          </pre>
        ))}
      </div>

      <form className="dev-console-prompt" onSubmit={handleSubmit}>
        <span className="dev-prompt-label mono">
          [{currentUser.username || currentUser.display_name}@ourchive] $
        </span>
        <input
          ref={inputRef}
          type="text"
          className="dev-prompt-input mono"
          spellCheck={false}
          autoComplete="off"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="help"
        />
      </form>
    </div>
  );
}
