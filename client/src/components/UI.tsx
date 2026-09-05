import { AlertCircle, Check, Info, LoaderCircle, X } from 'lucide-react';
import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from 'react';
import { Children, Fragment, isValidElement, useEffect, useId, useRef } from 'react';
import { createPortal } from 'react-dom';
import type { Toast } from '../types';

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  tone?: 'primary' | 'secondary' | 'ghost' | 'danger';
  size?: 'sm' | 'md' | 'icon';
  busy?: boolean | undefined;
};

export function Button({
  tone = 'secondary',
  size = 'md',
  busy,
  className = '',
  children,
  disabled,
  ...props
}: ButtonProps) {
  return (
    <button
      type="button"
      className={`button button--${tone} button--${size} ${className}`}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      {...props}
    >
      <span className={`button__content ${busy ? 'is-hidden' : ''}`}>{children}</span>
      {busy && <LoaderCircle className="button__spinner spin" size={16} aria-hidden />}
    </button>
  );
}

type FieldProps = {
  label: string;
  hint?: string | undefined;
  error?: string | undefined;
  optional?: boolean | undefined;
  children: ReactNode;
};

export function Field({ label, hint, error, optional, children }: FieldProps) {
  return (
    <label className={`field ${error ? 'field--error' : ''}`}>
      <span className="field__label">
        {label}
        {optional && <span>可选</span>}
      </span>
      {children}
      {(error || hint) && <span className="field__hint">{error ?? hint}</span>}
    </label>
  );
}

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={`input ${props.className ?? ''}`} />;
}

export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={`input select ${props.className ?? ''}`} />;
}

export function Textarea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={`input textarea ${props.className ?? ''}`} />;
}

type ModalProps = {
  open: boolean;
  title: string;
  description?: string | undefined;
  size?: 'sm' | 'md' | 'lg';
  variant?: 'form' | 'confirm' | 'settings';
  closeDisabled?: boolean;
  onClose: () => void;
  children?: ReactNode | undefined;
  footer?: ReactNode | undefined;
};

const modalStack: { instance: symbol; layer: HTMLDivElement }[] = [];

function updateModalLayers() {
  const top = modalStack.at(-1);
  for (const entry of modalStack) {
    const hidden = entry !== top;
    entry.layer.inert = hidden;
    if (hidden) entry.layer.setAttribute('aria-hidden', 'true');
    else entry.layer.removeAttribute('aria-hidden');
  }
}

function hasContent(content: ReactNode): boolean {
  return Children.toArray(content).some((child) => {
    if (isValidElement<{ children?: ReactNode }>(child) && child.type === Fragment) {
      return hasContent(child.props.children);
    }
    return child !== '';
  });
}

function focusableElements(container: HTMLElement): HTMLElement[] {
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])',
    ),
  ).filter(
    (element) =>
      !element.hidden && element.getAttribute('aria-hidden') !== 'true' && element.getClientRects().length > 0,
  );
}

export function Modal({
  open,
  title,
  description,
  size = 'md',
  variant = 'form',
  closeDisabled = false,
  onClose,
  children,
  footer,
}: ModalProps) {
  const titleId = useId();
  const descriptionId = useId();
  const layerRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const instanceRef = useRef(Symbol('modal'));
  const onCloseRef = useRef(onClose);

  useEffect(() => {
    onCloseRef.current = () => {
      if (!closeDisabled) onClose();
    };
  }, [onClose, closeDisabled]);

  useEffect(() => {
    if (!open || !layerRef.current) return undefined;
    const instance = instanceRef.current;
    const previous = document.activeElement as HTMLElement | null;
    const entry = { instance, layer: layerRef.current };
    modalStack.push(entry);
    updateModalLayers();
    const root = document.getElementById('root');
    if (root) root.inert = true;

    const onKey = (event: KeyboardEvent) => {
      if (modalStack.at(-1) !== entry) return;
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopImmediatePropagation();
        onCloseRef.current();
        return;
      }
      if (event.key !== 'Tab' || !panelRef.current) return;
      const focusable = focusableElements(panelRef.current);
      if (!focusable.length) {
        event.preventDefault();
        panelRef.current.focus();
        return;
      }
      const first = focusable[0];
      const last = focusable.at(-1);
      if (!first || !last) return;
      const focusIsOutside = !panelRef.current.contains(document.activeElement);
      if (event.shiftKey && (document.activeElement === first || focusIsOutside)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || focusIsOutside)) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKey, true);
    document.body.classList.add('modal-open');
    const focusTimer = window.setTimeout(() => {
      if (modalStack.at(-1) !== entry || !panelRef.current) return;
      const preferred = panelRef.current.querySelector<HTMLElement>(
        '[autofocus], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), button:not([data-modal-close]):not(:disabled)',
      );
      (preferred ?? panelRef.current).focus();
    }, 20);
    return () => {
      window.clearTimeout(focusTimer);
      document.removeEventListener('keydown', onKey, true);
      const wasTop = modalStack.at(-1) === entry;
      const index = modalStack.lastIndexOf(entry);
      if (index >= 0) modalStack.splice(index, 1);
      updateModalLayers();
      if (!modalStack.length) {
        document.body.classList.remove('modal-open');
        if (root) root.inert = false;
      }
      if (wasTop && previous?.isConnected) previous.focus();
    };
  }, [open]);

  if (!open) return null;
  return createPortal(
    <div
      ref={layerRef}
      className={`modal-layer modal-layer--${variant}`}
      role="presentation"
      onMouseDown={(event) => {
        if (event.currentTarget === event.target && modalStack.at(-1)?.instance === instanceRef.current) {
          onCloseRef.current();
        }
      }}
    >
      <div
        ref={panelRef}
        className={`modal modal--${size} modal--${variant}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
      >
        <header className="modal__header">
          <div>
            <h2 id={titleId}>{title}</h2>
            {description && <p id={descriptionId}>{description}</p>}
          </div>
          <Button
            data-modal-close
            size="icon"
            tone="ghost"
            disabled={closeDisabled}
            onClick={() => onCloseRef.current()}
            aria-label="关闭"
          >
            <X size={19} />
          </Button>
        </header>
        {hasContent(children) && <div className="modal__body">{children}</div>}
        {hasContent(footer) && <footer className="modal__footer">{footer}</footer>}
      </div>
    </div>,
    document.body,
  );
}

type ActionMenuProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  label: string;
  className?: string | undefined;
  trigger: ReactNode;
  children: ReactNode;
};

function enabledMenuItems(container: HTMLElement | null): HTMLElement[] {
  if (!container) return [];
  return Array.from(container.querySelectorAll<HTMLElement>('[role="menuitem"]')).filter(
    (item) => !item.hidden && item.getAttribute('aria-disabled') !== 'true' && !item.matches(':disabled'),
  );
}

export function ActionMenu({ open, onOpenChange, label, className = '', trigger, children }: ActionMenuProps) {
  const menuId = useId();
  const menuRef = useRef<HTMLDivElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const changeRef = useRef(onOpenChange);

  useEffect(() => {
    changeRef.current = onOpenChange;
  }, [onOpenChange]);

  useEffect(() => {
    if (!open) return undefined;
    enabledMenuItems(popoverRef.current ?? menuRef.current)[0]?.focus();

    const onPointerDown = (event: PointerEvent) => {
      if (!menuRef.current?.contains(event.target as Node)) changeRef.current(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopImmediatePropagation();
        changeRef.current(false);
        triggerRef.current?.focus();
        return;
      }
      if (
        !menuRef.current?.contains(event.target as Node) ||
        (event.key !== 'ArrowDown' && event.key !== 'ArrowUp' && event.key !== 'Home' && event.key !== 'End')
      )
        return;

      const items = enabledMenuItems(popoverRef.current ?? menuRef.current);
      if (!items.length) return;
      event.preventDefault();
      event.stopPropagation();
      const current = items.indexOf(document.activeElement as HTMLElement);
      const nextIndex =
        event.key === 'Home'
          ? 0
          : event.key === 'End'
            ? items.length - 1
            : event.key === 'ArrowDown'
              ? (current + 1) % items.length
              : current <= 0
                ? items.length - 1
                : current - 1;
      items[nextIndex]?.focus();
    };
    document.addEventListener('pointerdown', onPointerDown, true);
    document.addEventListener('keydown', onKeyDown, true);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown, true);
      document.removeEventListener('keydown', onKeyDown, true);
    };
  }, [open]);

  return (
    <div ref={menuRef} className={`action-menu ${className} ${open ? 'is-open' : ''}`}>
      <button
        type="button"
        ref={triggerRef}
        className="action-menu__trigger"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={menuId}
        onClick={() => {
          const nextOpen = !open;
          changeRef.current(nextOpen);
          if (!nextOpen) triggerRef.current?.focus();
        }}
      >
        {trigger}
      </button>
      <div ref={popoverRef} id={menuId} className="action-menu__popover" role="menu" hidden={!open}>
        {children}
      </div>
    </div>
  );
}

type ConfirmProps = {
  open: boolean;
  title: string;
  description: string;
  confirmLabel?: string;
  busy?: boolean;
  danger?: boolean;
  onCancel: () => void;
  onConfirm: () => void;
};

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = '确认',
  busy,
  danger,
  onCancel,
  onConfirm,
}: ConfirmProps) {
  return (
    <Modal
      open={open}
      title={title}
      description={description}
      size="sm"
      variant="confirm"
      closeDisabled={Boolean(busy)}
      onClose={onCancel}
      footer={
        <>
          <Button onClick={onCancel} disabled={busy}>
            取消
          </Button>
          <Button tone={danger ? 'danger' : 'primary'} busy={busy} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </>
      }
    />
  );
}

export function EmptyState({
  icon,
  title,
  description,
  action,
}: {
  icon: ReactNode;
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      <div className="empty-state__icon">{icon}</div>
      <h3>{title}</h3>
      <p>{description}</p>
      {action && <div className="empty-state__action">{action}</div>}
    </div>
  );
}

export function ToastStack({
  toasts,
  dismiss,
  className = '',
}: {
  toasts: Toast[];
  dismiss: (id: number) => void;
  className?: string | undefined;
}) {
  return (
    <div className={`toast-stack ${className}`} aria-live="polite">
      {toasts.map((toast) => (
        <div key={toast.id} className={`toast toast--${toast.tone}`}>
          {toast.tone === 'success' ? (
            <Check size={17} />
          ) : toast.tone === 'error' ? (
            <AlertCircle size={17} />
          ) : (
            <Info size={17} />
          )}
          <span>{toast.message}</span>
          <button onClick={() => dismiss(toast.id)} aria-label="关闭通知">
            <X size={15} />
          </button>
        </div>
      ))}
    </div>
  );
}
