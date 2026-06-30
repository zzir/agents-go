import { useState, useCallback, useRef, useEffect, type FormEvent, type KeyboardEvent, type ReactNode } from 'react';
import { IconButton } from '@primer/react';
import { PaperAirplaneIcon, SquareCircleIcon } from '@primer/octicons-react';

interface MessageInputProps {
  onSend: (text: string) => void;
  onCancel: () => void;
  disabled: boolean;
  running: boolean;
  toolbar?: ReactNode;
}

export function MessageInput({ onSend, onCancel, disabled, running, toolbar }: MessageInputProps) {
  const [text, setText] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const autoResize = useCallback(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 'px';
  }, []);

  useEffect(() => { autoResize(); }, [text, autoResize]);

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText('');
  };

  const handleKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      handleSubmit(e);
    }
  };

  return (
    <div className="chat-input-container">
      <form onSubmit={handleSubmit} className="chat-input-box">
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="type something here…"
          rows={1}
        />
        <div className="chat-input-toolbar">
          {toolbar}
          <div className="chat-input-toolbar-send">
            <span className="chat-input-divider" />
            {running ? (
              <IconButton
                icon={SquareCircleIcon}
                variant="invisible"
                aria-label="Stop"
                onClick={(e) => { e.preventDefault(); onCancel(); }}
                style={{ color: 'var(--fgColor-danger)' }}
              />
            ) : (
              <IconButton
                icon={PaperAirplaneIcon}
                variant="invisible"
                aria-label="Send"
                type="submit"
                disabled={disabled || !text.trim()}
              />
            )}
          </div>
        </div>
      </form>
    </div>
  );
}
