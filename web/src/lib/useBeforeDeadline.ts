import { useEffect, useState } from "react";

export function useBeforeDeadline(deadline: number) {
  const [expired, setExpired] = useState<number | null>(null);
  const before = Date.now() < deadline && expired !== deadline;

  useEffect(() => {
    if (!before) return;
    let timer: number;

    function check() {
      const left = deadline - Date.now();

      if (left <= 0) setExpired(deadline);
      // Dates can be farther away than the browser's maximum timer delay.
      else timer = window.setTimeout(check, Math.min(left, 2_147_483_647));
    }

    check();

    return () => window.clearTimeout(timer);
  }, [before, deadline]);

  return before;
}
