import { Component, type ReactNode } from "react";
import { matchRoutes } from "react-router-dom";
import { Button } from "./ui/Button";
import { EmptyState } from "./ui/EmptyState";

type Props = { resetKey: string; children: ReactNode };

type State = { failed: boolean; resetKey: string };

const RELOAD_MARK = "fold:page-chunk-reload";

// A page chunk that a link starts early on hover or focus and its route
// loads on navigation. React.lazy remembers a rejection for the life of the
// tab, so the route waits for an early start still on its way and tries once
// more after any failure before it settles. Some browsers (WebKit) keep a
// failed module for the whole document, so a second failure reloads the page
// once; the session mark stops a loop, and offline, without storage, or once
// the reader has moved to a page that doesn't need this chunk (`needed` is
// asked when the failure lands), the failure goes to RouteErrorBoundary.
type ChunkOptions = { needed: (pathname: string) => boolean; reload?: () => void };

export function pageChunk<T>(load: () => Promise<T>, { needed, reload = () => window.location.reload() }: ChunkOptions) {
  let started: Promise<T> | null = null;

  function warm() {
    if (started) return;
    const attempt = load();
    started = attempt;
    attempt.catch(() => {
      if (started === attempt) started = null;
    });
  }

  async function route(): Promise<T> {
    try {
      return loaded(await (started ?? load()));
    } catch {
      try {
        return loaded(await load());
      } catch (err) {
        if (needed(window.location.pathname) && reloadedOnce(reload)) return new Promise<T>(() => {});
        throw err;
      }
    }
  }

  return { warm, route, needed };
}

// Whether App's routes open `path` for an address rather than a sibling it
// overlaps, judged as React Router judges it: on the decoded address, with
// static segments first, so "/groups/%6Eew" opens "/groups/new".
export function opensRoute(path: string, siblings: string[]): (pathname: string) => boolean {
  const routes = [path, ...siblings].map((p) => ({ path: p }));

  return (pathname) => matchRoutes(routes, pathname)?.[0]?.route.path === path;
}

function loaded<T>(chunk: T): T {
  try {
    sessionStorage.removeItem(RELOAD_MARK);
  } catch {
    // Without storage no mark was ever set.
  }

  return chunk;
}

function reloadedOnce(reload: () => void): boolean {
  try {
    if (!navigator.onLine || sessionStorage.getItem(RELOAD_MARK)) return false;
    sessionStorage.setItem(RELOAD_MARK, "1");
  } catch {
    return false;
  }

  reload();

  return true;
}

// Pages load as separate chunks, and a chunk can fail to arrive (Fold was
// updated since this tab opened, or the network dropped). Any page that
// throws gets the same way out, and the error's own text is never shown.
// Moving to another address clears it, without remounting the routes.
export class RouteErrorBoundary extends Component<Props, State> {
  state: State = { failed: false, resetKey: this.props.resetKey };

  static getDerivedStateFromError(): Partial<State> {
    return { failed: true };
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey === state.resetKey) return null;

    return { failed: false, resetKey: props.resetKey };
  }

  render() {
    if (!this.state.failed) return this.props.children;

    return (
      <div className="fold-auth">
        <EmptyState
          level={1}
          title="This page didn't load"
          body="Fold may have been updated since you opened it, or your connection dropped. Reload to get the latest version."
          action={
            <Button variant="primary" layout="w-full sm:w-auto" onClick={() => window.location.reload()}>
              Reload page
            </Button>
          }
        />
      </div>
    );
  }
}
