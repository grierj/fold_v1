import { useId, useRef, useState, type Ref } from "react";
import { flushSync } from "react-dom";
import type { Visibility } from "../types";
import { useAction, type Action } from "../lib/useAction";
import { coverSrc } from "../lib/covers";
import { useUnsavedChanges } from "../lib/unsavedChanges";
import { useFocusHome, useLockedFocus } from "../lib/focusHome";
import { Form } from "./ui/Form";
import { Checkbox, Choices, Field, FormError, Input, Radio, Textarea } from "./ui/Field";
import { FormActions } from "./ui/FormActions";
import { Button } from "./ui/Button";
import { ImageUpload } from "./ImageUpload";
import { ChevronDownIcon } from "./icons";

export interface GroupFormData {
  name: string;
  description: string;
  location: string;
  tags: string[];
  coverFile?: File | null;
  removeCover?: boolean;
  coverFocal?: { x: number; y: number };
  visibility: Visibility;
  memberLimit: number | null;
}

export function GroupForm({
  initial,
  saveLabel,
  onSaved,
  onCancel,
  saveRef,
  action: shared,
  locked = false,
}: {
  initial: {
    name: string; description: string; location: string; tags: string[];
    imageUrl: string; coverFocalX: number; coverFocalY: number;
    // The id the no-photo pattern comes from (coverSrc); 0 for a group with no id yet.
    coverId: number;
    visibility?: Visibility;
    memberLimit?: number | null;
    // Tags, member limit and cover start open when editing, closed on create (DEC-100).
    moreOpen: boolean;
  };
  saveLabel: string;
  // Resolve false to say nothing was saved (the leader still has a review to confirm).
  onSaved: (data: GroupFormData) => Promise<void | false>;
  onCancel: () => void;
  // The submit button, for a review that hands focus back to it.
  saveRef?: Ref<HTMLButtonElement>;
  // A run the page shares with another step of the same save (the review's confirm), so only one goes at a time.
  action?: Action;
  locked?: boolean;
}) {
  const [name, setName] = useState(initial.name);
  const [description, setDescription] = useState(initial.description);
  const [location, setLocation] = useState(initial.location);
  const [tags, setTags] = useState(initial.tags.join(", "));
  const [removeCover, setRemoveCover] = useState(false);
  const [coverFile, setCoverFile] = useState<File | null>(null);
  const [coverFocal, setCoverFocal] = useState<{ x: number; y: number } | null>(null);
  const [visibility, setVisibility] = useState<Visibility>(initial.visibility ?? "public");
  const initialLimit = initial.memberLimit ? String(initial.memberLimit) : "";
  const [memberLimit, setMemberLimit] = useState(initialLimit);
  const [moreOpen, setMoreOpen] = useState(initial.moreOpen);
  const limitRef = useRef<HTMLInputElement>(null);
  const moreId = useId();
  const summaryId = useId();
  const own = useAction();
  const action = shared ?? own;
  const frozen = action.running || locked;
  const error = action.state.status === "failed" ? action.state.text : null;
  // Nothing changes while a save is on its way: it sends what was there when it began, and a success closes the
  // form, so a later edit would be lost without a word.
  const home = useFocusHome<HTMLFormElement>();
  useLockedFocus(home, frozen);

  const dirty = name !== initial.name || description !== initial.description || location !== initial.location
    || tags !== initial.tags.join(", ") || visibility !== (initial.visibility ?? "public") || memberLimit !== initialLimit
    || coverFile !== null || coverFocal !== null || removeCover;

  const { guard, markSaved } = useUnsavedChanges(dirty);
  // What a closed "More options" still tells you, so nothing in it is a surprise.
  const tagList = tags.split(",").map((t) => t.trim()).filter(Boolean);
  const hasPhoto = !!coverFile || (!!initial.imageUrl && !removeCover);

  const moreSummary = [
    tagList.length ? `Tags: ${tagList.join(", ")}` : "No tags",
    memberLimit.trim() ? `Limit ${memberLimit.trim()}` : "No member limit",
    hasPhoto ? "Photo cover" : "Pattern cover",
  ].join(" · ");

  return (
    <Form ref={home.ref} tabIndex={-1} noValidate
      className="fold-form space-y-8"
      // A bad member limit inside the closed section opens it before the form
      // checks its fields, so the error shows and focus can land on the field.
      onSubmitCapture={() => {
        if (!moreOpen && limitRef.current && !limitRef.current.validity.valid) flushSync(() => setMoreOpen(true));
      }}
      onSubmit={(e) => {
        e.preventDefault();

        if (frozen) return;
        void action.run(async () => {
          const saved = await onSaved({
            name,
            description,
            location,
            coverFile,
            removeCover: !coverFile && removeCover,
            coverFocal: coverFocal ?? undefined,
            tags: tags
              .split(",")
              .map((t) => t.trim())
              .filter(Boolean),
            visibility,
            memberLimit: memberLimit.trim() ? Number(memberLimit) : null,
          });

          if (saved === false) return false;
          markSaved();
        }, { fallback: "We couldn't save your changes." });
      }}
    >
      {guard}
      {error && <FormError>{error}</FormError>}
      {/* pointer-events: the cover's focal point is set by a click on the image, which `disabled` doesn't stop. */}
      <fieldset disabled={frozen} className="min-w-0 space-y-8 disabled:pointer-events-none">
        <section className="space-y-4">
          <h2 className="text-xl font-semibold">About the group</h2>
          <Field id="group-name" label="Group name" hint="Up to 200 characters.">
            <Input value={name} onChange={(e) => setName(e.target.value)} required />
          </Field>
          <Field id="group-description" label="Description" optional hint="Up to 5,000 characters.">
            <Textarea rows={4} value={description} onChange={(e) => setDescription(e.target.value)} />
          </Field>
          <Field id="group-location" label="Location" optional hint="Where it usually meets, for example Room 201, Main Campus. Up to 300 characters.">
            <Input value={location} onChange={(e) => setLocation(e.target.value)} />
          </Field>
        </section>
        <section className="fold-section">
          <Choices legend="Who can see this group">
            <Radio
              name="visibility"
              label="Public"
              description="Anyone can see it, including people who are logged out."
              checked={visibility === "public"}
              onChange={() => setVisibility("public")}
            />
            <Radio
              name="visibility"
              label="Private"
              description="Only signed-in people can see it."
              checked={visibility === "private"}
              onChange={() => setVisibility("private")}
            />
            <Radio
              name="visibility"
              label="Invite only"
              description="Hidden from everyone except its members and church admins. People join only when a leader adds them."
              checked={visibility === "invite_only"}
              onChange={() => setVisibility("invite_only")}
            />
          </Choices>
        </section>
        <section className="fold-section">
          <Button variant="link" aria-expanded={moreOpen} aria-controls={moreId} aria-describedby={moreOpen ? undefined : summaryId} onClick={() => setMoreOpen(!moreOpen)}>
            More options
            <ChevronDownIcon className={`ml-1 size-5 ${moreOpen ? "rotate-180" : ""}`} />
          </Button>
          {!moreOpen && <p id={summaryId} className="text-sm text-text-muted">{moreSummary}</p>}
          {/* Hidden, not unmounted: what's typed or picked here survives closing it. */}
          <div id={moreId} hidden={!moreOpen} className="mt-4 space-y-6">
            <Field id="group-tags" label="Tags" optional hint="Separate tags with commas, for example young-adults, bible-study. Use up to 500 characters total.">
              <Input value={tags} onChange={(e) => setTags(e.target.value)} />
            </Field>
            <Field id="member-limit" label="Member limit" optional hint="Enter a whole number from 1 to 100,000, or leave it blank for no limit. When the group is full, people can’t join on their own. Leaders can still add someone.">
              <Input ref={limitRef} type="number" min={1} max={100000} step={1} value={memberLimit} onChange={(e) => setMemberLimit(e.target.value)} layout="sm:w-44" />
            </Field>
            <div>
              <ImageUpload
                label="Cover image"
                initialUrl={initial.imageUrl || undefined}
                placeholderSrc={coverSrc({ id: initial.coverId })}
                focal={coverFocal ?? { x: initial.coverFocalX, y: initial.coverFocalY }}
                maxDim={1600}
                aspectClass="aspect-video max-w-sm"
                onFile={setCoverFile}
                onFocal={setCoverFocal}
              />
              {initial.imageUrl && !coverFile && (
                <div className="mt-2">
                  <Checkbox label="Remove the current cover" checked={removeCover} onChange={(e) => setRemoveCover(e.target.checked)} />
                </div>
              )}
            </div>
          </div>
        </section>
      </fieldset>
      <FormActions>
        <Button ref={saveRef} variant="primary" layout="w-full sm:w-auto" type="submit" disabled={frozen}>
          {action.running ? "Saving…" : saveLabel}
        </Button>
        <Button variant="secondary" layout="w-full sm:w-auto" disabled={frozen} onClick={onCancel}>
          Cancel
        </Button>
      </FormActions>
    </Form>
  );
}

export function CreateGroupForm({ onSaved, onCancel }: {
  onSaved: (data: GroupFormData) => Promise<void>;
  onCancel: () => void;
}) {
  return (
    <GroupForm
      initial={{
        name: "", description: "", location: "", tags: [], imageUrl: "",
        // No id yet, so the cover pattern is the one coverSrc gives id 0.
        coverId: 0, moreOpen: false,
        coverFocalX: 50, coverFocalY: 50, visibility: "public",
      }}
      saveLabel="Create group"
      onSaved={onSaved}
      onCancel={onCancel}
    />
  );
}

