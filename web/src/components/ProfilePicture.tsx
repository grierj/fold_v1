import { useState } from "react";
import { Profile } from "../api/profile";
import { publicName } from "../lib/can";
import { errorText } from "../lib/errors";
import { useAction } from "../lib/useAction";
import type { User } from "../types";
import { ImageUpload } from "./ImageUpload";
import { Button } from "./ui/Button";
import { FormError } from "./ui/Field";

type Point = { x: number; y: number };

type Crop =
  | { status: "saving"; url: string; point: Point }
  | { status: "done"; url: string }
  | { status: "failed"; text: string };

type FocalSave = { next: Point | null; settled: Promise<void> };

// One write at a time per picture, including when the editor is reopened.
const focalSaves = new Map<string, FocalSave>();

function savePosition(url: string, point: Point, refresh: () => Promise<void>) {
  const running = focalSaves.get(url);

  if (running) {
    running.next = point;

    return running.settled;
  }

  const save: FocalSave = {
    next: point,
    settled: Promise.resolve().then(async () => {
      try {
        do {
          try {
            while (save.next) {
              const next = save.next;
              save.next = null;
              await Profile.update({ avatarFocal: { ...next, avatarUrl: url } });
            }
          } finally {
            await refresh();
          }
        } while (save.next);
      } finally {
        focalSaves.delete(url);
      }
    }),
  };

  focalSaves.set(url, save);

  return save.settled;
}

export function ProfilePicture({ user, refresh }: { user: User; refresh: () => Promise<void> }) {
  const [crop, setCrop] = useState<Crop | null>(null);
  const file = useAction();
  const url = user.avatarUrl;
  const adjusting = crop?.status === "saving" && crop.url === url;
  const focal = adjusting ? crop.point : { x: user.avatarFocalX, y: user.avatarFocalY };

  async function adjust(point: Point) {
    if (!url || file.running || (focal.x === point.x && focal.y === point.y)) return;
    const edit: Crop = { status: "saving", url, point };
    setCrop(edit);

    try {
      await savePosition(url, point, refresh);
      setCrop((current) => current === edit ? { status: "done", url } : current);
    } catch (error) {
      setCrop((current) => current === edit ? { status: "failed", text: errorText(error, "We couldn't save your picture position.") } : current);
    }
  }

  function upload(picture: File | null) {
    if (!picture || adjusting) return;
    setCrop(null);
    void file.run(async () => {
      await Profile.uploadAvatar(picture);

      return "Picture saved.";
    }, { fallback: "We couldn't save your picture.", followUp: refresh });
  }

  function remove() {
    if (adjusting || !confirm("Remove your profile picture?")) return;
    setCrop(null);
    void file.run(async () => {
      await Profile.deleteAvatar();

      return "Picture removed.";
    }, { fallback: "We couldn't remove that.", followUp: refresh });
  }

  const locked = adjusting || file.running;
  const saved = crop ? crop.status === "done" && crop.url === url && "Picture saved." : file.state.status === "done" && file.state.text;
  const status = locked ? "Saving picture…" : saved || "";
  const error = crop?.status === "failed" ? crop.text : !crop && file.state.status === "failed" && file.state.text;

  return <div className="space-y-4">
    <ImageUpload key={url} label="Profile picture" initialUrl={url || undefined} focal={focal} maxDim={512} aspectClass="aspect-square max-w-48" placeholderName={publicName(user)} onFile={upload} onFocal={adjust} previewSelection={false} fileLocked={locked} focalLocked={file.running} />
    <p role="status" className={!status ? "sr-only" : locked ? "text-text-muted text-sm" : "text-success text-sm"}>{status}</p>
    {error && <FormError layout="mt-3">{error}</FormError>}
    <p className="text-xs text-text-muted mt-3">A new picture is saved as soon as you choose it.</p>
    {url && <Button variant="danger-outline" layout="mt-3" onClick={remove} disabled={locked}>Remove picture</Button>}
  </div>;
}
