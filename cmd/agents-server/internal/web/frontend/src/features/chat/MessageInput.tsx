import { useState, useCallback, useRef, useEffect, useMemo, type FormEvent, type KeyboardEvent, type ReactNode } from 'react';
import { IconButton } from '@primer/react';
import { PaperAirplaneIcon, SquareCircleIcon } from '@primer/octicons-react';
import { loadDraft, saveDraft, clearDraft } from '@/lib/drafts';
import { onComposerInsert } from '@/lib/composer';
import { SlashCommandPopup, matchCommands, slashOptionID, slashQuery, useSlashCommands, type SlashCommand } from '@/features/chat/SlashMenu';

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

  // The slash commands: offered while the box holds nothing but a command
  // prefix, narrowed as it is typed, walked with the arrow keys and taken
  // with Enter or Tab — the way "/" works in an editor. Escape, or leaving
  // the box, dismisses the offer until the text changes.
  const commands = useSlashCommands();
  const query = slashQuery(text);
  const offered = useMemo(() => (query === null ? [] : matchCommands(commands, query)), [commands, query]);
  const [dismissedFor, setDismissedFor] = useState<string | null>(null);
  const [activeIndex, setActiveIndex] = useState(0);
  const popupOpen = query !== null && offered.length > 0 && dismissedFor !== text;
  useEffect(() => { setActiveIndex(0); }, [query]);
  const pick = useCallback((cmd: SlashCommand) => {
    updateText(cmd.insert);
    textareaRef.current?.focus();
  }, [updateText]);

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText('');
    clearDraft(sessionId);
  };

  const handleKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    // While an IME composition is active (Chinese/Japanese/Korean input),
    // Enter commits the candidate selection and must NOT send the message.
    // `isComposing` is set for the whole composition; keyCode 229 is the
    // legacy signal browsers emit for the same in-composition key.
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
    if (popupOpen) {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          setActiveIndex(i => (i + 1) % offered.length);
          return;
        case 'ArrowUp':
          e.preventDefault();
          setActiveIndex(i => (i - 1 + offered.length) % offered.length);
          return;
        case 'Enter':
        case 'Tab':
          e.preventDefault();
          pick(offered[Math.min(activeIndex, offered.length - 1)]);
          return;
        case 'Escape':
          e.preventDefault();
          setDismissedFor(text);
          return;
      }
    }
    if (e.key === 'Enter' && !e.shiftKey) {
      handleSubmit(e);
    }
  };

  return (
    <div className="chat-input-container">
      <form onSubmit={handleSubmit} className="chat-input-box">
        <SlashCommandPopup open={popupOpen} commands={offered} activeIndex={activeIndex} onPick={pick} />
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(e) => updateText(e.target.value)}
          onKeyDown={handleKeyDown}
          onBlur={() => { if (popupOpen) setDismissedFor(text); }}
          placeholder="type something here…"
          rows={2}
          aria-autocomplete="list"
          aria-controls={popupOpen ? 'slash-commands' : undefined}
          aria-activedescendant={popupOpen ? slashOptionID(Math.min(activeIndex, offered.length - 1)) : undefined}
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
