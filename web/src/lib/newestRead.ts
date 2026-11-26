import { useCallback, useRef } from "react";

// A read asked again before it answers, for a screen that decides what it offers from whether its read landed
// (the teen consent screens). Only the newest ask's answer is applied; an older one that lands later is
// dropped. Every caller, an older one too, settles with the newest ask's outcome: useLoad's reload settles a
// replaced ask as "stale", which can't tell its caller whether what is on screen is current.
export function useNewestRead<T>(fetch: () => Promise<T>, apply: (data: T) => void): () => Promise<void> {
  const newest = useRef<Promise<void> | null>(null);
  const latest = useRef({ fetch, apply });
  latest.current = { fetch, apply };

  return useCallback(() => {
    const mine: Promise<void> = latest.current.fetch().then((data) => {
      if (newest.current === mine) latest.current.apply(data);
    });

    newest.current = mine;

    const settle = async (ask: Promise<void>): Promise<void> => {
      try {
        await ask;
      } catch (cause) {
        if (newest.current === ask) throw cause;
      }

      if (newest.current !== ask && newest.current) await settle(newest.current);
    };

    return settle(mine);
  }, []);
}
