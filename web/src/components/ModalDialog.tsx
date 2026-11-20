/**
 * ModalDialog — Reusable accessible native `<dialog>` wrapper with Escape/backdrop light-dismiss
 * and ARIA title binding.
 */
import { ReactNode, useEffect, useId, useRef } from "react";
import { X } from "lucide-react";

interface ModalDialogProps {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
}

export function ModalDialog({ open, title, onClose, children }: ModalDialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      dialog.showModal();
    } else if (!open && dialog.open) {
      dialog.close();
    }
  }, [open]);

  // Fallback for browsers (e.g. Safari) without native <dialog closedby="any"> support.
  useEffect(() => {
    const dialog = ref.current;
    if (!dialog || "closedBy" in HTMLDialogElement.prototype) return;

    const handleClick = (event: MouseEvent) => {
      if (event.target !== dialog) return;
      const rect = dialog.getBoundingClientRect();
      const inside =
        rect.top <= event.clientY &&
        event.clientY <= rect.top + rect.height &&
        rect.left <= event.clientX &&
        event.clientX <= rect.left + rect.width;
      if (!inside) {
        dialog.close();
      }
    };

    dialog.addEventListener("click", handleClick);
    return () => dialog.removeEventListener("click", handleClick);
  }, []);

  return (
    <dialog
      ref={ref}
      className="modal-dialog"
      aria-labelledby={titleId}
      onClose={onClose}
      {...({ closedby: "any" } as Record<string, string>)}
    >
      <div className="modal-head">
        <h2 id={titleId} className="modal-title">
          {title}
        </h2>
        <button type="button" className="icon-btn" aria-label="Close dialog" onClick={() => ref.current?.close()}>
          <X size={16} strokeWidth={1.6} />
        </button>
      </div>
      <div className="modal-body">{children}</div>
    </dialog>
  );
}
