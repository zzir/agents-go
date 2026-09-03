import { useState, useCallback, useRef, useEffect, useMemo, type FormEvent, type KeyboardEvent, type ClipboardEvent, type ReactNode } from 'react';
import { ActionList, ActionMenu, IconButton, Spinner } from '@primer/react';
import { ImageIcon, PaperAirplaneIcon, PlusIcon, SquareCircleIcon, XIcon, SyncIcon } from '@primer/octicons-react';
import { loadDraft, saveDraft, clearDraft, loadAttachmentDraft, saveAttachmentDraft } from '@/lib/drafts';
import { onComposerInsert } from '@/lib/composer';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { fetchAttachmentConfig, imageAffordance, uploadAttachment, isImageFile, type AttachmentMeta, type AttachmentConfig } from '@/lib/attachments';
import { SlashCommandPopup, matchCommands, slashOptionID, slashQuery, useSlashCommands, type SlashCommand } from '@/features/chat/SlashMenu';

interface MessageInputProps {
  sessionId: string;
  onSend: (text: string, attachments?: AttachmentMeta[]) => void;
  onCancel: (graceful?: boolean) => void;
  disabled: boolean;
  running: boolean;
  // allowAttachments gates every image affordance: attachment storage is
  // configured AND the picked agent has Vision on.
  allowAttachments?: boolean;
  toolbar?: ReactNode;
  // plusItems is what the "+" menu offers after Image — the Project submenu
  // while the session is unbound; null once bound. The button renders only
  // when something in the menu can be taken.
  plusItems?: ReactNode;
}

// One image in the composer strip: uploading (localUrl preview), ready
// (server meta), or failed (kept for retry).
interface AttachmentDraft {
  key: string;
  file: File;
  localUrl: string;
  status: 'uploading' | 'ready' | 'error';
  meta?: AttachmentMeta;
}

let draftKey = 0;

export function MessageInput({ sessionId, onSend, onCancel, disabled, running, allowAttachments, toolbar, plusItems }: MessageInputProps) {
  const [text, setText] = useState(() => loadDraft(sessionId));
  const [atts, setAtts] = useState<AttachmentDraft[]>([]);
  const [attCfg, setAttCfg] = useState<AttachmentConfig | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let alive = true;
    fetchAttachmentConfig().then(cfg => { if (alive) setAttCfg(cfg); });
    return () => { alive = false; };
  }, []);

  // Restore the session's saved attachment draft (already-uploaded images
  // survive a session switch; in-flight or failed ones do not).
  useEffect(() => {
    const saved = loadAttachmentDraft(sessionId);
    setAtts(saved.map(meta => ({ key: `saved-${meta.id}`, file: null as unknown as File, localUrl: meta.url, status: 'ready' as const, meta })));
  }, [sessionId]);

  const syncAttDraft = useCallback((list: AttachmentDraft[]) => {
    saveAttachmentDraft(sessionId, list.filter(a => a.status === 'ready' && a.meta).map(a => a.meta!));
  }, [sessionId]);

  const startUpload = useCallback((draft: AttachmentDraft, cfg: AttachmentConfig) => {
    uploadAttachment(draft.file, cfg).then(meta => {
      setAtts(prev => { const next = prev.map(a => a.key === draft.key ? { ...a, status: 'ready' as const, meta } : a); syncAttDraft(next); return next; });
    }).catch(err => {
      toast.error(`Image upload failed: ${err instanceof Error ? err.message : err}`);
      setAtts(prev => prev.map(a => a.key === draft.key ? { ...a, status: 'error' as const } : a));
    });
  }, [syncAttDraft]);

  const addFiles = useCallback((files: File[]) => {
    const cfg = attCfg;
    if (!cfg?.enabled || !allowAttachments) {
      if (files.some(isImageFile)) toast.info(!cfg?.enabled ? 'Image attachments are not configured on this server' : 'This agent does not accept images — enable Vision in its settings');
      return;
    }
    const images = files.filter(isImageFile);
    if (files.length > images.length) toast.info('Only png and jpeg images are accepted');
    if (images.length === 0) return;
    setAtts(prev => {
      const room = Math.max(0, cfg.max_count - prev.length);
      if (images.length > room) toast.error(`A message carries at most ${cfg.max_count} images`);
      const added = images.slice(0, room).map(f => ({
        key: `up-${++draftKey}`, file: f, localUrl: URL.createObjectURL(f), status: 'uploading' as const,
      }));
      added.forEach(d => startUpload(d, cfg));
      return [...prev, ...added];
    });
  }, [attCfg, allowAttachments, startUpload]);

  const removeAtt = useCallback((key: string) => {
    setAtts(prev => {
      const gone = prev.find(a => a.key === key);
      if (gone?.meta) void api.attachments.remove(gone.meta.id).catch(() => {});
      if (gone?.localUrl.startsWith('blob:')) URL.revokeObjectURL(gone.localUrl);
      const next = prev.filter(a => a.key !== key);
      syncAttDraft(next);
      return next;
    });
  }, [syncAttDraft]);

  const handlePaste = useCallback((e: ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(e.clipboardData?.files ?? []);
    if (files.length === 0) return;
    e.preventDefault();
    addFiles(files);
    // Mixed clipboards (a screenshot tool offering text + image) keep the text.
    const txt = e.clipboardData.getData('text/plain');
    if (txt) setText(prev => { const next = prev + txt; saveDraft(sessionId, next); return next; });
  }, [addFiles, sessionId]);

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

  const uploading = atts.some(a => a.status === 'uploading');
  const readyAtts = atts.filter(a => a.status === 'ready' && a.meta).map(a => a.meta!);
  const image = imageAffordance(attCfg, Boolean(allowAttachments));
  const showPlus = image.enabled || plusItems != null;

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (disabled || uploading) return;
    if (!trimmed && readyAtts.length === 0) return;
    onSend(trimmed, readyAtts.length ? readyAtts : undefined);
    setText('');
    clearDraft(sessionId);
    atts.forEach(a => { if (a.localUrl.startsWith('blob:')) URL.revokeObjectURL(a.localUrl); });
    setAtts([]);
    saveAttachmentDraft(sessionId, []);
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
        {atts.length > 0 && (
          <div className="chat-attachment-strip">
            {atts.map(a => (
              <div key={a.key} className={'chat-attachment-chip' + (a.status === 'error' ? ' is-error' : '')}>
                <img src={a.status === 'ready' && a.meta ? a.meta.url : a.localUrl} alt="" />
                {a.status === 'uploading' && <span className="chat-attachment-busy"><Spinner size="small" /></span>}
                {a.status === 'error' && attCfg && (
                  <IconButton className="chat-attachment-retry" icon={SyncIcon} size="small" variant="invisible" aria-label="Retry upload"
                    onClick={(e) => { e.preventDefault(); setAtts(prev => prev.map(x => x.key === a.key ? { ...x, status: 'uploading' } : x)); startUpload(a, attCfg); }} />
                )}
                <IconButton className="chat-attachment-remove" icon={XIcon} size="small" variant="invisible" aria-label="Remove image"
                  onClick={(e) => { e.preventDefault(); removeAtt(a.key); }} />
              </div>
            ))}
          </div>
        )}
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(e) => updateText(e.target.value)}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
          onBlur={() => { if (popupOpen) setDismissedFor(text); }}
          placeholder="type something here…"
          rows={2}
          aria-autocomplete="list"
          aria-controls={popupOpen ? 'slash-commands' : undefined}
          aria-activedescendant={popupOpen ? slashOptionID(Math.min(activeIndex, offered.length - 1)) : undefined}
        />
        <input
          ref={fileInputRef}
          type="file"
          accept="image/png,image/jpeg"
          multiple
          hidden
          onChange={(e) => { addFiles(Array.from(e.target.files ?? [])); e.target.value = ''; }}
        />
        <div className="chat-input-toolbar">
          {showPlus && (
            <ActionMenu>
              <ActionMenu.Anchor>
                <IconButton icon={PlusIcon} size="small" variant="invisible" aria-label="Add" />
              </ActionMenu.Anchor>
              <ActionMenu.Overlay>
                <ActionList>
                  <ActionList.Item disabled={!image.enabled} onSelect={() => fileInputRef.current?.click()}>
                    <ActionList.LeadingVisual><ImageIcon /></ActionList.LeadingVisual>
                    Image…
                    {image.hint && <ActionList.Description variant="block">{image.hint}</ActionList.Description>}
                  </ActionList.Item>
                  {plusItems}
                </ActionList>
              </ActionMenu.Overlay>
            </ActionMenu>
          )}
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
                disabled={disabled || uploading || (!text.trim() && readyAtts.length === 0)}
              />
            )}
          </div>
        </div>
      </form>
    </div>
  );
}
