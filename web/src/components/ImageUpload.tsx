import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { focalPosition, prepareUpload } from "../lib/image";
import { Avatar } from "./Avatar";
import { Button } from "./ui/Button";
import { Field, FormError, HiddenFileInput, Input } from "./ui/Field";

// With no image, keep the compact chooser (DEC-99).
export function ImageUpload({
  label,
  initialUrl,
  focal,
  maxDim,
  aspectClass,
  onFile,
  onFocal,
  previewSelection = true,
  fileLocked,
  focalLocked,
  placeholderSrc,
  placeholderName,
}: {
  label: string;
  initialUrl?: string;
  focal: { x: number; y: number };
  maxDim: number;
  aspectClass: string;
  onFile: (file: File | null) => void;
  onFocal: (f: { x: number; y: number }) => void;
  previewSelection?: boolean;
  fileLocked?: boolean;
  focalLocked?: boolean;
  // The stand-in shown while there is no image: what people will see instead
  // (a group's pattern cover), or the person's initials for an avatar.
  placeholderSrc?: string;
  placeholderName?: string;
}) {
  const [selection, setSelection] = useState<string | null>(null);
  const preview = selection ?? initialUrl;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const active = useRef(false);
  const canAdjust = !busy && !focalLocked;

  useLayoutEffect(() => {
    active.current = true;

    return () => { active.current = false; };
  }, []);

  useEffect(() => {
    return () => {
      if (selection) URL.revokeObjectURL(selection);
    };
  }, [selection]);

  async function pick(e: React.ChangeEvent<HTMLInputElement>) {
    const f = e.target.files?.[0];
    e.target.value = "";

    if (!f || busy || fileLocked) return;
    setBusy(true);
    setError(null);

    try {
      const resized = await prepareUpload(f, maxDim);

      if (!active.current) return;

      if (previewSelection) setSelection(URL.createObjectURL(resized));
      onFile(resized);
    } catch {
      setError("Could not process that image.");
    } finally {
      setBusy(false);
    }
  }

  function clear() {
    setSelection(null);
    onFile(null);
  }

  function pointFromClick(e: React.MouseEvent<HTMLDivElement>) {
    if (!canAdjust) return;
    const rect = e.currentTarget.getBoundingClientRect();
    onFocal({
      x: Math.min(100, Math.max(0, Math.round(((e.clientX - rect.left) / rect.width) * 100))),
      y: Math.min(100, Math.max(0, Math.round(((e.clientY - rect.top) / rect.height) * 100))),
    });
  }

  const chooser = (
    <>
      <Button variant="secondary" onClick={() => inputRef.current?.click()} disabled={busy || fileLocked}>
        {busy ? "Processing…" : preview ? "Choose different image" : "Choose photo"}
      </Button>
      <HiddenFileInput ref={inputRef} accept="image/jpeg,image/png,image/gif" onChange={pick} />
    </>
  );

  if (!preview) {
    return (
      <div>
        <p className="label">{label}</p>
        <div className="flex items-center gap-4">
          {placeholderSrc ? (
            <img src={placeholderSrc} alt="" className="w-16 h-16 shrink-0 rounded-md object-cover bg-sunken border border-border" />
          ) : placeholderName ? (
            <Avatar name={placeholderName} size="w-16 h-16" textSize="text-2xl" />
          ) : (
            <div aria-hidden="true" className="w-16 h-16 shrink-0 rounded-md bg-sunken border border-border" />
          )}
          {chooser}
        </div>
        {error && <FormError layout="mt-1">{error}</FormError>}
      </div>
    );
  }

  return (
    <div>
      <p className="label">{label}</p>
      <div
        className={`relative ${aspectClass} w-full overflow-hidden rounded-md bg-sunken border border-border cursor-crosshair`}
        onClick={pointFromClick}
        title="Click to set the focal point"
      >
        <img
          src={preview}
          alt=""
          className="w-full h-full object-cover pointer-events-none"
          style={{ objectPosition: focalPosition(focal.x, focal.y) }}
        />
        <div
          className="absolute w-6 h-6 -ml-3 -mt-3 rounded-full border-2 border-inverse-foreground ring-1 ring-inverse shadow-sm pointer-events-none"
          style={{ left: `${focal.x}%`, top: `${focal.y}%` }}
        />
      </div>
      <p className="text-xs text-text-muted mt-1">
        Tap the image or use the sliders to move the focal point. Faces stay centered when cropped.
      </p>
      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <Field label="Focal point from left">
          <Input type="range" min={0} max={100} disabled={!canAdjust} value={focal.x} aria-valuetext={`${focal.x}% from the left`} onChange={(e) => onFocal({ ...focal, x: e.target.valueAsNumber })} />
        </Field>
        <Field label="Focal point from top">
          <Input type="range" min={0} max={100} disabled={!canAdjust} value={focal.y} aria-valuetext={`${focal.y}% from the top`} onChange={(e) => onFocal({ ...focal, y: e.target.valueAsNumber })} />
        </Field>
      </div>
      <div className="flex gap-2 mt-2">
        {chooser}
        {previewSelection && <Button variant="secondary" onClick={clear}>Clear</Button>}
      </div>
      {error && <FormError layout="mt-1">{error}</FormError>}
    </div>
  );
}
