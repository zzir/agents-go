import { useState, useCallback, useRef, useEffect, type FormEvent, type KeyboardEvent, type ReactNode } from 'react';
import { IconButton } from '@primer/react';
import { PaperAirplaneIcon, SquareCircleIcon } from '@primer/octicons-react';
import { loadDraft, saveDraft, clearDraft } from '@/lib/drafts';
import { onComposerInsert } from '@/lib/composer';

interface MessageInputProps {
  sessionId: string;
  onSend: (text: string) => void;
  onCancel: (graceful?: boolean) => void;
  disabled: boolean;
  running: boolean;
  toolbar?: ReactNode;
}

export function MessageInput({ sessionId, onSend, onCancel, disabled, running, toolbar }: MessageInputProps) {
  const [text, setText] = useState(() => loadDraft(sessionId));
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const updateText = useCallback((v: string) => {
    setText(v);
    saveDraft(sessionId, v);
  }, [sessionId]);

  const autoResize = useCallback(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 'px';
  }, []);

  useEffect(() => { autoResize(); }, [text, autoResize]);

  // Receive text injected from elsewhere in the app (e.g. terminal output
  // quoted via the panel's quote button): append on its own line and focus.
  useEffect(() => {
    onComposerInsert(injected => {
      setText(prev => {
        const next = prev ? (prev.endsWith('\n') ? prev : prev + '\n') + injected : injected;
        saveDraft(sessionId, next);
        return next;
      });
      textareaRef.current?.focus();
    });
    return () => onComposerInsert(null);
  }, [sessionId]);

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText('');
    clearDraft(sessionId);
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
          onChange={(e) => updateText(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="type something here…"
          rows={2}
        />
        <div className="chat-input-toolbar">
          {toolbar}
          <div className="chat-input-toolbar-send">
            <span className="chat-input-divider" />
            {running ? (
              <IconButton
                icon={SquareCircleIcon}
                variant="invisible"
                aria-label="Stop (Shift-click to finish the current turn first)"
                onClick={(e) => { e.preventDefault(); onCancel(e.shiftKey); }}
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
