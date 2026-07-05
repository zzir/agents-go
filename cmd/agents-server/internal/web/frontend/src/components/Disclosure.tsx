import { useState, type ReactNode, type Ref } from 'react';
import { ChevronRightIcon } from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import './disclosure.css';

type Variant = 'default' | 'accent' | 'success' | 'attention' | 'severe' | 'danger' | 'done' | 'open' | 'closed';

interface DisclosureProps {
  icon?: Icon;
  label: ReactNode;
  variant?: Variant;
  as?: 'button' | 'div';
  defaultOpen?: boolean;
  forceOpen?: boolean;
  open?: boolean;
  onToggle?: () => void;
  className?: string;
  ref?: Ref<HTMLDivElement>;
  children: ReactNode;
}

export function Disclosure({
  icon: IconCmp, label, variant = 'default', as: Tag = 'button',
  defaultOpen = false, forceOpen, open: controlledOpen, onToggle,
  className, ref, children,
}: DisclosureProps) {
  const [uncontrolled, setUncontrolled] = useState(defaultOpen);
  const isControlled = controlledOpen !== undefined;
  const expanded = forceOpen || (isControlled ? controlledOpen : uncontrolled);
  const handleClick = isControlled ? onToggle : () => setUncontrolled(o => !o);

  return (
    <div
      ref={ref}
      data-variant={variant}
      className={'disclosure' + (expanded ? ' expanded' : '') + (className ? ' ' + className : '')}
    >
      <Tag className="disclosure-header" onClick={handleClick}>
        <ChevronRightIcon size={14} className="disclosure-chevron" />
        {IconCmp && <span className="disclosure-icon"><IconCmp size={14} /></span>}
        <span className="disclosure-label">{label}</span>
      </Tag>
      {expanded && <div className="disclosure-body">{children}</div>}
    </div>
  );
}
