import { useEffect, useRef, useState } from "react";
import { LIST_SHOWN } from "../lib/roomLabels";
import { Button } from "./ui/Button";

// A long list shows its first rows and the rest behind one "Show all" button. The button goes once it is pressed, so
// focus moves to the first row it showed, and the keyboard keeps its place in the list.
export function useShowAll<T>(items: T[]) {
  const [all, setAll] = useState(false);
  const firstShown = useRef<HTMLLIElement>(null);
  const pressed = useRef(false);

  useEffect(() => {
    if (!all || !pressed.current) return;
    pressed.current = false;
    firstShown.current?.focus();
  }, [all]);

  return {
    rows: all ? items : items.slice(0, LIST_SHOWN),
    // What row `i` takes: the first row the button shows can be given focus.
    row: (i: number) => (i === LIST_SHOWN ? { ref: firstShown, tabIndex: -1 } : {}),
    button: !all && items.length > LIST_SHOWN
      ? <Button variant="link" onClick={() => { pressed.current = true; setAll(true); }}>Show all {items.length}</Button>
      : null,
  };
}
