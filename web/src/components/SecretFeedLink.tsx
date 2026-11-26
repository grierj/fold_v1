import { useEffect, useRef, useState, type DependencyList, type ReactNode } from "react";
import { errorText } from "../lib/errors";
import { formatEventDay } from "../lib/when";
import { useLoad, type Load } from "../lib/useLoad";
import { useAction } from "../lib/useAction";
import { Button } from "./ui/Button";
import { Field, FormError, Input } from "./ui/Field";
import { Skeleton } from "./ui/Skeleton";
import { ErrorNotice } from "./ErrorNotice";

export interface FeedInfo {
  hasToken: boolean;
  createdAt?: string | null;
  // Present only in the answer that made the link.
  url?: string | null;
}

export function useSecretFeed<T extends FeedInfo>({ load, deps, create, revoke, failed, onInfo, onRevoked, recover }: {
  load: () => Promise<T>;
  deps: DependencyList;
  create: (current: T) => Promise<T>;
  revoke: (current: T) => Promise<T>;
  failed: { load: string; create: string; revoke: string };
  // The personal feed takes its scope from the read or the link it just made.
  onInfo?: (info: T) => void;
  onRevoked?: () => void;
  recover?: (cause: unknown, retry: Load<T>["retry"]) => Promise<boolean>;
}) {
  const feed = useLoad(load, deps);
  const action = useAction();
  const [status, setStatus] = useState("");
  const linkRef = useRef<HTMLInputElement>(null);
  const [applied, setApplied] = useState(feed.data);

  if (onInfo && feed.data && feed.data !== applied) {
    setApplied(feed.data);
    onInfo(feed.data);
  }

  const current = feed.data;
  const link = current?.url ?? null;

  const act = (run: () => Promise<void>, fallback: string) => action.run(async () => {
    setStatus("");
    await run();
  }, { fallback });

  function change(write: (current: T) => Promise<T>, fallback: string, onSaved?: (info: T) => void) {
    if (!current) return Promise.resolve();

    return act(async () => {
      const epoch = feed.epoch();
      let info: T;

      try {
        info = await write(current);
      } catch (cause) {
        if (feed.epoch() !== epoch || await recover?.(cause, feed.retry)) return;

        throw cause;
      }

      if (feed.epoch() !== epoch) return;

      if (onInfo) setApplied(info);

      feed.setData(info);
      onSaved?.(info);
    }, fallback);
  }

  const make = () => change(create, failed.create, onInfo);

  useEffect(() => {
    if (link) linkRef.current?.focus();
  }, [link]);

  return {
    loaded: current !== undefined,
    loadError: feed.error ? errorText(feed.error, failed.load) : null,
    retry: feed.retry,
    hasToken: current?.hasToken ?? false,
    createdAt: current?.createdAt ?? null,
    link,
    busy: action.running,
    error: action.state.status === "failed" ? action.state.text : null,
    status, linkRef, act, setStatus,
    create: make,
    remake: (confirmText: string) => confirm(confirmText) ? make() : Promise.resolve(),
    turnOff: (confirmText: string) => confirm(confirmText) ? change(revoke, failed.revoke, () => {
      setStatus("Calendar link turned off.");
      onRevoked?.();
    }) : Promise.resolve(),
    async copyLink() {
      if (!link) return;

      if (action.state.status === "failed") action.reset();

      if (navigator.clipboard) {
        try {
          await navigator.clipboard.writeText(link);
          setStatus("Link copied.");

          return;
        } catch {
          // Fall through to selecting the text by hand.
        }
      }

      linkRef.current?.select();
      setStatus("The link is selected. Copy it from the box above.");
    },
  };
}

export type SecretFeed = ReturnType<typeof useSecretFeed>;

export function SecretFeedLink({ feed, id, loadingLabel, linkLabel, linkHint, setUp, none, confirmRemake, confirmOff, extras, after }: {
  feed: SecretFeed;
  id: string;
  loadingLabel: string;
  linkLabel: string;
  linkHint: string;
  // Said when a link exists but cannot be shown again.
  setUp: (madeOn: string | null) => ReactNode;
  none: ReactNode;
  confirmRemake: string;
  confirmOff: string;
  // Controls between the error line and the link (the personal feed's scope).
  extras?: ReactNode;
  // Rendered below the buttons, above the status line.
  after?: ReactNode;
}) {
  return <>
    {feed.error && <FormError>{feed.error}</FormError>}
    {feed.loadError ? (
      <ErrorNotice message={feed.loadError} onRetry={feed.retry} />
    ) : !feed.loaded ? (
      <Skeleton kind="lines" count={2} label={loadingLabel} />
    ) : (
      <>
        {extras}
        {feed.link ? (
          <Field id={id} label={linkLabel} hint={linkHint}>
            <Input ref={feed.linkRef} value={feed.link} readOnly layout="font-mono text-sm" onFocus={(e) => e.currentTarget.select()} />
          </Field>
        ) : feed.hasToken ? (
          <p className="text-sm text-text-muted">{setUp(feed.createdAt ? formatEventDay(feed.createdAt) : null)}</p>
        ) : (
          <p className="text-sm text-text-muted">{none}</p>
        )}
        <div className="flex flex-wrap gap-2">
          {feed.hasToken ? (
            <>
              {feed.link && (
                <Button variant="primary" layout="w-full sm:w-auto" onClick={() => void feed.copyLink()}>
                  Copy link
                </Button>
              )}
              <Button variant="secondary" layout="w-full sm:w-auto" onClick={() => void feed.remake(confirmRemake)} disabled={feed.busy}>
                {feed.busy ? "Saving…" : "Make a new link"}
              </Button>
              <Button variant="danger-outline" layout="w-full sm:w-auto" onClick={() => void feed.turnOff(confirmOff)} disabled={feed.busy}>
                Turn off calendar link
              </Button>
            </>
          ) : (
            <Button variant="primary" layout="w-full sm:w-auto" onClick={() => void feed.create()} disabled={feed.busy}>
              {feed.busy ? "Saving…" : "Create calendar link"}
            </Button>
          )}
        </div>
      </>
    )}
    {after}
    <p role="status" className="text-success text-sm empty:hidden">{feed.status}</p>
  </>;
}
