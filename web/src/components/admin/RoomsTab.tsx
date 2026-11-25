import { Fragment, useEffect, useRef, useState } from "react";
import { ApiError } from "../../api/core";
import { AdminRooms, type RoomInput } from "../../api/rooms";
import type { AdminRoomsResponse, Room, RoomResponse } from "../../types";
import type { Load } from "../../lib/useLoad";
import { RefusedSaveLine, useRefusedSave } from "../../lib/refusedSave";
import { answerNames, useRevisionSave, type RevisionConflict, type RevisionRead } from "../../lib/useRevisionSave";
import { usePage } from "../../lib/usePage";
import { PageCount, Pagination } from "../ui/Pagination";
import { SearchBox } from "../ui/SearchBox";
import { Menu } from "../ui/Menu";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { EmptyState } from "../ui/EmptyState";
import { Checkbox, Field, FormError, focusFirstInvalid, Input, Select, Textarea } from "../ui/Field";
import { Form } from "../ui/Form";
import { FormActions } from "../ui/FormActions";
import { ErrorNotice } from "../ErrorNotice";
import { RoomFeedPanel } from "./RoomFeedPanel";
import { Skeleton } from "../ui/Skeleton";
import { errorCode, errorText } from "../../lib/errors";
import { useUnsavedChanges } from "../../lib/unsavedChanges";

// The room catalog (M2.1a). Active rooms first, then building and name, the
// same order the server sends.
export function sortRooms(rooms: Room[]): Room[] {
  return [...rooms].sort((a, b) =>
    Number(b.active) - Number(a.active) || a.building.localeCompare(b.building) || a.name.localeCompare(b.name));
}

const details = (r: Room) =>
  [r.building, r.capacity !== null ? `Seats ${r.capacity}` : "", r.features].filter(Boolean).join(" · ");

interface RoomForm {
  name: string;
  building: string;
  capacity: string;
  features: string;
  notes: string;
  restricted: boolean;
  setup: string;
  cleanup: string;
}

const EMPTY: RoomForm = { name: "", building: "", capacity: "", features: "", notes: "", restricted: false, setup: "", cleanup: "" };

const fromRoom = (r: Room): RoomForm => ({
  name: r.name,
  building: r.building,
  capacity: r.capacity === null ? "" : String(r.capacity),
  features: r.features,
  notes: r.notes,
  restricted: r.restricted,
  setup: String(r.defaultSetupMinutes),
  cleanup: String(r.defaultTeardownMinutes),
});

// What an edit of a room writes, as PUT /admin/rooms/:id stores it: the text trimmed, a blank capacity as no limit and
// blank padding as none. Active is not the editor's to write: Deactivate and Reactivate send it on their own.
export const roomFields = (r: Room) => ({
  name: r.name.trim(),
  building: r.building.trim(),
  capacity: r.capacity,
  features: r.features.trim(),
  notes: r.notes.trim(),
  restricted: r.restricted,
  defaultSetupMinutes: r.defaultSetupMinutes,
  defaultTeardownMinutes: r.defaultTeardownMinutes,
});

const minutes = (value: string) => (value.trim() === "" ? undefined : Number(value));

// The body an edit sends: every field, so a cleared capacity goes as null and a cleared padding goes back to 0.
const editBody = (form: RoomForm): RoomInput => ({
  name: form.name.trim(),
  building: form.building.trim(),
  features: form.features.trim(),
  notes: form.notes.trim(),
  restricted: form.restricted,
  capacity: form.capacity.trim() === "" ? null : Number(form.capacity),
  defaultSetupMinutes: minutes(form.setup) ?? 0,
  defaultTeardownMinutes: minutes(form.cleanup) ?? 0,
});

// The body a new room sends: blank fields are left to the server's defaults.
function createBody(form: RoomForm): RoomInput {
  const body: RoomInput = {
    name: form.name.trim(),
    building: form.building.trim(),
    features: form.features.trim(),
    notes: form.notes.trim(),
    restricted: form.restricted,
  };
  const setup = minutes(form.setup);
  const cleanup = minutes(form.cleanup);

  if (form.capacity.trim() !== "") body.capacity = Number(form.capacity);

  if (setup !== undefined) body.defaultSetupMinutes = setup;

  if (cleanup !== undefined) body.defaultTeardownMinutes = cleanup;

  return body;
}

// Every send was refused while nothing the save writes changed: the control says its ordinary refusal.
const refusedEverySend = () => new ApiError(409, "room changed", { code: "room_changed" });

// The room as one read of the list shows it, or undefined when the list has no such room.
async function readRoom(id: number, keep: (list: AdminRoomsResponse) => void): Promise<RevisionRead<Room | undefined>> {
  const list = await AdminRooms.list();
  keep(list);
  const room = list.rooms.find((r) => r.id === id);

  return { allowed: room !== undefined, revision: room?.revision, value: room };
}

// RoomEditor is one opening of a room's edit form. It mounts once per opening: the save names the read the form opened
// with (DEC-171), and a refusal is the shared protocol's (useRevisionSave). On a conflict the form closes and the tab
// reads the rooms from nothing (`onConflict`).
function RoomEditor({ room, onSaved, onConflict, onCancel }: {
  room: Room;
  onSaved: (room: Room) => void;
  onConflict: (why: RevisionConflict) => void;
  onCancel: () => void;
}) {
  const [opened] = useState(room);
  const lastAnswer = useRef<RoomResponse | null>(null);
  const lastRead = useRef<Room | undefined>(undefined);
  const answered = () => lastAnswer.current;
  const found = () => lastRead.current;

  const send = useRevisionSave<Room | undefined, ReturnType<typeof roomFields> | null>({
    opened,
    revision: opened.revision,
    read: () => readRoom(opened.id, (list) => { lastRead.current = list.rooms.find((r) => r.id === opened.id); }),
    fields: (r) => (r ? roomFields(r) : null),
    answers: answerNames("room", opened.id),
    onConflict,
    changedCode: "room_changed",
  });

  async function submit(form: RoomForm): Promise<Room | null> {
    const body = editBody(form);
    lastAnswer.current = null;

    const intended: Room = {
      ...opened,
      ...body,
      capacity: body.capacity ?? null,
      defaultSetupMinutes: body.defaultSetupMinutes ?? 0,
      defaultTeardownMinutes: body.defaultTeardownMinutes ?? 0,
    };

    const sent = await send(async (revision) => {
      const res = await AdminRooms.update(opened.id, body, revision);
      lastAnswer.current = res;

      return res;
    }, intended);

    if (sent === "refused") throw refusedEverySend();

    if (sent !== "saved") return null;

    // Saved: by its answer, or by a read that found the room as this save would leave it.
    return answered()?.room ?? found() ?? intended;
  }

  return <RoomFormPanel room={opened} submit={submit} onSaved={onSaved} onCancel={onCancel} />;
}

function RoomFormPanel({ room, submit, onSaved, onCancel }: {
  room?: Room;
  // Saves the form: the room as stored, or null when the save was refused and its editor is closing.
  submit: (form: RoomForm) => Promise<Room | null>;
  onSaved: (room: Room) => void;
  onCancel: () => void;
}) {
  const initial = room ? fromRoom(room) : EMPTY;
  const [form, setForm] = useState<RoomForm>(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nameError, setNameError] = useState<string | null>(null);
  const formEl = useRef<HTMLFormElement>(null);
  const mounted = useRef(true);
  // SAFETY: initial contains only the RoomForm fields we build from the room.
  const dirty = (Object.keys(initial) as (keyof RoomForm)[]).some((k) => form[k] !== initial[k]);
  const { guard, markSaved } = useUnsavedChanges(dirty);
  const set = <K extends keyof RoomForm>(key: K, value: RoomForm[K]) => setForm((f) => ({ ...f, [key]: value }));

  useEffect(() => {
    mounted.current = true;

    return () => { mounted.current = false; };
  }, []);

  // The button that opened the form is gone, so focus starts on the first field.
  useEffect(() => {
    formEl.current?.querySelector<HTMLElement>("input, textarea, select")?.focus();
  }, []);

  // The name error renders after the failed save; focus its field then.
  useEffect(() => {
    if (nameError) focusFirstInvalid(formEl.current);
  }, [nameError]);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setNameError(null);

    try {
      const saved = await submit(form);

      if (!saved) return;

      // A save that lands after the form went (the tab was left) still updates the list.
      if (mounted.current) markSaved();
      onSaved(saved);
    } catch (err) {
      if (!mounted.current) return;

      if (errorCode(err) === "room_name_taken") setNameError(errorText(err));
      else setError(errorText(err, "We couldn't save that room."));
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  const idPrefix = room ? `room-${room.id}` : "room-new";

  return (
    <Form ref={formEl} aria-label={room ? `Edit ${room.name}` : "Add room"} noValidate onSubmit={save}
      className="bg-surface rounded-lg border border-border p-6 space-y-4 max-w-xl">
      {guard}
      {error && <FormError>{error}</FormError>}
      <Field id={`${idPrefix}-name`} label="Name" error={nameError}>
        <Input value={form.name} maxLength={100} required
          onChange={(e) => { set("name", e.target.value); setNameError(null); }} />
      </Field>
      <Field id={`${idPrefix}-building`} label="Building" optional>
        <Input value={form.building} maxLength={100} onChange={(e) => set("building", e.target.value)} />
      </Field>
      <Field id={`${idPrefix}-capacity`} label="Capacity" optional hint="Leave blank for no limit">
        <Input type="number" inputMode="numeric" min={1} max={100000} step={1} layout="w-28"
          value={form.capacity} onChange={(e) => set("capacity", e.target.value)} />
      </Field>
      <Field id={`${idPrefix}-features`} label="Features" optional hint="For example: projector, piano, kitchen">
        <Input value={form.features} maxLength={500} onChange={(e) => set("features", e.target.value)} />
      </Field>
      <Field id={`${idPrefix}-notes`} label="Notes for admins" optional hint="Only admins see this">
        <Textarea rows={3} value={form.notes} maxLength={2000} onChange={(e) => set("notes", e.target.value)} />
      </Field>
      <Checkbox id={`${idPrefix}-restricted`} label="Only admins can book this room" description="For rooms like the nursery"
        checked={form.restricted} onChange={(e) => set("restricted", e.target.checked)} />
      <Field id={`${idPrefix}-setup`} label="Setup minutes" optional hint="Held before each booking. Leave blank for none.">
        <Input type="number" inputMode="numeric" min={0} max={240} step={1} layout="w-28"
          value={form.setup} onChange={(e) => set("setup", e.target.value)} />
      </Field>
      <Field id={`${idPrefix}-cleanup`} label="Cleanup minutes" optional hint="Held after each booking. Leave blank for none.">
        <Input type="number" inputMode="numeric" min={0} max={240} step={1} layout="w-28"
          value={form.cleanup} onChange={(e) => set("cleanup", e.target.value)} />
      </Field>
      <FormActions>
        <Button variant="primary" layout="w-full sm:w-auto" type="submit" disabled={busy}>
          {busy ? "Saving…" : room ? "Save room" : "Add room"}
        </Button>
        <Button variant="secondary" layout="w-full sm:w-auto" disabled={busy} onClick={onCancel}>Cancel</Button>
      </FormActions>
    </Form>
  );
}

// Deactivate and Reactivate send only `active`, so that is all they compare: someone else's edit of the room's other
// fields is no reason to refuse them.
const activeOf = (r: Room | undefined) => (r ? { active: r.active } : null);

// One room in the list, with its Deactivate or Reactivate. That is a control that stays on screen (DEC-171): it names
// the read the row shows, and after each outcome the next press names what that outcome left (`stays`). A conflict
// shows the rooms as its read found them and says so at the row.
function RoomRow({ room, locked, feedOpen, onBusy, onStart, onEdit, onFeed, onCloseFeed, onSaved, onShown, onStatus, onGone }: {
  room: Room;
  // A form or panel is open, or a save is on its way: nothing here may start.
  locked: boolean;
  feedOpen: boolean;
  onBusy: (busy: boolean) => void;
  // Deactivate or Reactivate was pressed: what the tab said about an earlier action goes.
  onStart: () => void;
  onEdit: () => void;
  onFeed: () => void;
  onCloseFeed: () => void;
  onSaved: (room: Room) => void;
  onShown: (list: AdminRoomsResponse) => void;
  onStatus: (status: string) => void;
  // This person may no longer manage rooms.
  onGone: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const refusal = useRefusedSave();
  const lastAnswer = useRef<RoomResponse | null>(null);
  const lastRead = useRef<AdminRoomsResponse | null>(null);
  const answered = () => lastAnswer.current;
  const readList = () => lastRead.current;

  const save = useRevisionSave<Room | undefined, { active: boolean } | null>({
    opened: room,
    revision: room.revision,
    read: () => readRoom(room.id, (list) => { lastRead.current = list; }),
    fields: activeOf,
    answers: answerNames("room", room.id),
    stays: () => lastAnswer.current?.room,
    changedCode: "room_changed",
    onConflict: (why) => {
      if (why === "gone") {
        onGone();

        return;
      }

      if (lastRead.current) onShown(lastRead.current);
      refusal.refused(why);
    },
  });

  async function setActive(active: boolean) {
    onBusy(true);
    setError(null);
    refusal.clear();
    onStart();
    lastAnswer.current = null;
    lastRead.current = null;
    const verb = active ? "reactivate" : "deactivate";

    try {
      const sent = await save(async (revision) => {
        const res = await AdminRooms.update(room.id, { active }, revision);
        lastAnswer.current = res;

        return res;
      }, { ...room, active });

      if (sent === "refused") throw refusedEverySend();

      if (sent !== "saved") return;
      // Saved: by its answer, or by a read that found the room as this press would leave it.
      const answer = answered();
      const list = readList();

      if (answer) onSaved(answer.room);
      else if (list) onShown(list);
      onStatus(`“${answer?.room.name ?? list?.rooms.find((r) => r.id === room.id)?.name ?? room.name}” ${verb}d.`);
    } catch (e) {
      setError(errorText(e, `We couldn't ${verb} that room.`));
    } finally {
      onBusy(false);
    }
  }

  return (
    <div role="group" aria-label={room.name} className="border-b border-border py-3">
      {error && <FormError>{error}</FormError>}
      <RefusedSaveLine save={refusal} noun="room" choosing="This room" />
      <div className="flex items-center gap-3 flex-wrap">
        <div className="grow min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-bold">{room.name}</span>
            {room.restricted && <Badge>Restricted</Badge>}
            {!room.active && <Badge>Inactive</Badge>}
          </div>
          {details(room) && <p className="text-sm text-text-muted mt-0.5">{details(room)}</p>}
        </div>
        <Button variant="secondary" data-opener={room.id} aria-label={`Edit ${room.name}`} disabled={locked}
          onClick={() => { refusal.clear(); onEdit(); }}>Edit</Button>
        <div data-feed-opener={room.id}><Menu tone="quiet" label={`More for ${room.name}`} items={[
          ...(room.active ? [{ label: `Calendar feed for ${room.name}`, disabled: locked, onSelect: () => { refusal.clear(); onFeed(); } }] : []),
          { label: `${room.active ? "Deactivate" : "Reactivate"} ${room.name}`, disabled: locked, onSelect: () => void setActive(!room.active) },
        ]} /></div>
      </div>
      {feedOpen && room.active && <div className="mt-4"><RoomFeedPanel room={room} onClose={onCloseFeed} /></div>}
    </div>
  );
}

// The list lives in AdminPage (it also feeds the tab count), so a save hands the room back and the one list updates.
// `load` is that list's read: a refused edit closes its form and reads it from nothing (useRefusedSave).
export function RoomsTab({ rooms, error, reload, load, onSaved, onShown }: {
  rooms: Room[] | null;
  error: string | null;
  reload: () => void;
  load: Pick<Load<unknown>, "loading" | "data" | "retry">;
  onSaved: (room: Room) => void;
  // A refused save's read of the list: the page shows it.
  onShown: (list: AdminRoomsResponse) => void;
}) {
  // "new" is the add form; a number is the room being edited.
  const [open, setOpen] = useState<"new" | number | null>(null);
  const [query, setQuery] = useState("");
  const [building, setBuilding] = useState("");
  const [status, setStatus] = useState("");
  const [busyId, setBusyId] = useState<number | null>(null);
  const statusRef = useRef<HTMLParagraphElement>(null);
  const rootEl = useRef<HTMLDivElement>(null);
  // Which opener button gets focus back after Cancel: "new" or a room id.
  const [returnTo, setReturnTo] = useState<"new" | number | null>(null);
  // The room whose calendar-feed panel is open (M2.2b), and whose
  // "Calendar feed" button gets focus back when it closes.
  const [feedOpen, setFeedOpen] = useState<number | null>(null);
  const [feedReturn, setFeedReturn] = useState<number | null>(null);
  const refusal = useRefusedSave(load);
  // The room whose save was refused, which the line names.
  const [refusedId, setRefusedId] = useState<{ id: number; name: string } | null>(null);
  // While a form or panel is open, switching would drop its edits, and while a save is on its way its answer must reach
  // the room it was made for: the openers, the filters and the pages wait.
  const locked = open !== null || feedOpen !== null || busyId !== null;

  useEffect(() => {
    if (feedReturn === null || feedOpen !== null) return;
    rootEl.current?.querySelector<HTMLElement>(`[data-feed-opener="${feedReturn}"] button`)?.focus();
    setFeedReturn(null);
  }, [feedReturn, feedOpen]);

  function closeFeed() {
    setFeedReturn(feedOpen);
    setFeedOpen(null);
  }

  useEffect(() => {
    if (returnTo === null || open !== null) return;
    rootEl.current?.querySelector<HTMLElement>(`[data-opener="${returnTo}"]`)?.focus();
    setReturnTo(null);
  }, [returnTo, open]);

  function cancel() {
    setReturnTo(open);
    setOpen(null);
  }

  // The form and its trigger go away on save, so focus moves to the line that says what happened.
  useEffect(() => {
    if (status) statusRef.current?.focus();
  }, [status]);

  function saved(room: Room, verb: string) {
    onSaved(room);
    setOpen(null);
    setStatus(`“${room.name}” ${verb}.`);
  }

  // A save was refused: whatever was open closes, and the page reads the rooms from nothing and says so.
  function refused(room: Room, why: RevisionConflict) {
    setOpen(null);
    setFeedOpen(null);
    setStatus("");
    setRefusedId({ id: room.id, name: room.name });
    refusal.refused(why);
  }

  function opening() {
    refusal.clear();
    setRefusedId(null);
    setStatus("");
  }

  const ordered = [...(rooms ?? [])].sort((a, b) => a.building.localeCompare(b.building) || Number(b.active) - Number(a.active) || a.name.localeCompare(b.name));
  const matching = ordered.filter((r) => (!building || r.building === building) && `${r.name} ${r.building} ${r.features}`.toLowerCase().includes(query.trim().toLowerCase()));
  const paging = usePage(matching, query + building);
  const refusedName = refusedId ? rooms?.find((r) => r.id === refusedId.id)?.name ?? refusedId.name : undefined;
  const line = <RefusedSaveLine save={refusal} noun="room" canManage={rooms ? true : undefined} about={refusedName} />;

  if (!rooms) return <>{line}{error ? <ErrorNotice message={error} onRetry={reload} /> : <Skeleton kind="lines" />}</>;

  function startAdding() {
    opening();
    setOpen("new");
  }

  return (
    <div ref={rootEl} className="space-y-3">
      {line}
      {/* Always rendered so screen readers announce the text when it arrives; empty, it takes no room. */}
      <p ref={statusRef} role="status" tabIndex={-1} className={status ? "text-sm text-text-muted focus:outline-none" : "sr-only"}>{status}</p>
      {open === "new" ? (
        <RoomFormPanel submit={async (form) => (await AdminRooms.create(createBody(form))).room} onSaved={(r) => saved(r, "added")} onCancel={cancel} />
      ) : rooms.length > 0 && (
        <div><Button variant="secondary" data-opener="new" disabled={locked} onClick={startAdding}>Add room</Button></div>
      )}
      {rooms.length > 0 && <div className="flex flex-wrap items-end gap-3">
        <div className="min-w-48 flex-1"><SearchBox label="Search rooms" value={query} onChange={setQuery} disabled={locked} /></div>
        <Field label="Building"><Select value={building} onChange={(e) => setBuilding(e.target.value)} disabled={locked} options={[{ value: "", label: "All buildings" }, ...[...new Set(rooms.map((r) => r.building).filter(Boolean))].sort().map((b) => ({ value: b, label: b }))]} /></Field>
      </div>}
      <PageCount paging={paging} noun="rooms" />
      {rooms.length > 0 && matching.length === 0 && <EmptyState title="No rooms match" body="Try another name or choose another building." />}
      {paging.rows.map((r, index) => <Fragment key={r.id}>
        {(index === 0 || paging.rows[index - 1].building !== r.building) && <h2 className="pt-4 font-bold">{r.building || "Other rooms"}</h2>}
        {open === r.id ? (
          <RoomEditor key={r.id} room={r} onSaved={(saved_) => saved(saved_, "saved")} onConflict={(why) => refused(r, why)} onCancel={cancel} />
        ) : (
          <RoomRow key={r.id} room={r} locked={locked} feedOpen={feedOpen === r.id}
            onBusy={(busy) => setBusyId(busy ? r.id : null)}
            onStart={opening}
            onEdit={() => { opening(); setOpen(r.id); }}
            onFeed={() => { opening(); setFeedOpen(r.id); }}
            onCloseFeed={closeFeed}
            onSaved={onSaved}
            onShown={onShown}
            onStatus={setStatus}
            onGone={() => refused(r, "gone")} />
        )}</Fragment>)}
      <Pagination paging={paging} noun="rooms" disabled={locked} />
      {rooms.length === 0 && open !== "new" && (
        <EmptyState
          title="No rooms yet"
          body="Add the rooms your church books, like the fellowship hall or classrooms."
          action={<Button variant="primary" data-opener="new" onClick={startAdding}>Add room</Button>}
        />
      )}
    </div>
  );
}
