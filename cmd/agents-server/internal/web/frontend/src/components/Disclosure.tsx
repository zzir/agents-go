import { useId, useState, type KeyboardEvent, type ReactNode, type Ref } from 'react';
import { ChevronRightIcon } from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import './disclosure.css';

/** Primer color roles, plus 'plain' — an inline toggle with no card chrome. */
type Variant = 'default' | 'accent' | 'success' | 'attention' | 'severe' | 'danger' | 'done' | 'open' | 'closed' | 'plain';

interface DisclosureProps {
  icon?: Icon;
  label: ReactNode;
  variant?: Variant;
  /** 'div' when the label nests interactive content (invalid inside a
   *  <button>); the div header stays a keyboard-operable role=button. */
  as?: 'button' | 'div';
  defaultOpen?: boolean;
  forceOpen?: boolean;
  open?: boolean;
  onToggle?: () => void;
  className?: string;
  /** Scroll target id, emitted as data-anchor-id — what the Context panel jumps to. */
  anchorId?: string;
  ref?: Ref<HTMLDivElement>;
  children: ReactNode;
}

export function Disclosure({
  icon: IconCmp, label, variant = 'default', as = 'button',
  defaultOpen = false, forceOpen, open: controlledOpen, onToggle,
  className, anchorId, ref, children,
}: DisclosureProps) {
  const [uncontrolled, setUncontrolled] = useState(defaultOpen);
  const isControlled = controlledOpen !== undefined;
  const expanded = forceOpen || (isControlled ? controlledOpen : uncontrolled);
  const toggle = isControlled ? onToggle : () => setUncontrolled(o => !o);
  const bodyId = useId();
  // Enter/Space toggle a div header, but only when the key lands on the header
  // itself — a control nested in the label handles its own keys.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget || (e.key !== 'Enter' && e.key !== ' ')) return;
    e.preventDefault();
    toggle?.();
  };
  const header = (
    <>
      <ChevronRightIcon size={14} className="disclosure-chevron" />
      {IconCmp && <span className="disclosure-icon"><IconCmp size={14} /></span>}
      <span className="disclosure-label">{label}</span>
    </>
  );

  return (
    <div
      ref={ref}
      data-variant={variant}
      data-anchor-id={anchorId}
      className={'disclosure' + (expanded ? ' expanded' : '') + (className ? ' ' + className : '')}
    >
      {as === 'div' ? (
        <div className="disclosure-header" role="button" tabIndex={0} aria-expanded={expanded} aria-controls={bodyId} onClick={toggle} onKeyDown={onKeyDown}>
          {header}
        </div>
      ) : (
        <button type="button" className="disclosure-header" aria-expanded={expanded} aria-controls={bodyId} onClick={toggle}>
          {header}
        </button>
      )}
      {expanded && <div id={bodyId} className="disclosure-body">{children}</div>}
    </div>
  );
}
