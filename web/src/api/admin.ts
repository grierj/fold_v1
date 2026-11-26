import { isNumber } from "../lib/wireValues";
import type {
  AdminBackupStatus,
  AdminJobStatusResponse,
  AdminJobsResponse,
  GroupListResponse,
  GroupResponse,
  SettingAddressResponse,
  SettingOnResponse,
  SignedUpCount,
  TimezoneResponse,
} from "../types";
import { api, path, query, type Id } from "./core";

export const Admin = {
  pending: () => api<GroupListResponse>("/admin/pending"),
  // Tags are sent only when the admin edited them; left out, the leader's own stay.
  // fromNotification: the inline Approve on a pending group (the only allow-listed action, 6.6).
  approve: (id: Id, tags?: string[], fromNotification?: number) =>
    api<GroupResponse>(path`/admin/groups/${id}/approve`, { method: "POST", body: { tags, fromNotification } }),
  // The optional message goes to the leaders on their notice (DEC-96).
  reject: (id: Id, message?: string) =>
    api<unknown>(path`/admin/groups/${id}/reject`, { method: "POST", body: message ? { message } : undefined }),
  deletions: () => api<GroupListResponse>("/admin/deletions"),
  rejectDeletion: (id: Id) => api<unknown>(path`/admin/groups/${id}/reject-deletion`, { method: "POST" }),
  // answering: the Approvals card's delete, which answers the leader's request and is refused (already_handled) once
  // the request is no longer open. The Groups tab's delete sends no marker and is never refused for it.
  deleteGroup: (id: Id, answering?: boolean) =>
    api<unknown>(`${path`/admin/groups/${id}`}${query({ deletionRequest: answering })}`, { method: "DELETE" }),
  // How many other people deleting the group would notify, for its confirmation (DEC-177).
  deleteCount: (id: Id) => api<SignedUpCount>(path`/admin/groups/${id}/delete-count`),
  timezone: () => api<TimezoneResponse>("/admin/timezone"),
  setTimezone: (timezone: string, revision: string | undefined) => api<TimezoneResponse>("/admin/timezone", { method: "PUT", body: { timezone, revision } }),
  // The church's mailing address for email footers (DEC-116).
  churchAddress: () => api<SettingAddressResponse>("/admin/settings/church-address"),
  setChurchAddress: (address: string, revision: string | undefined) =>
    api<SettingAddressResponse>("/admin/settings/church-address", { method: "PUT", body: { address, revision } }),
  // Guardian copies of messages to teens (DEC-19) and where replies go (DEC-130).
  guardianCopies: () => api<SettingOnResponse>("/admin/settings/guardian-copies"),
  setGuardianCopies: (on: boolean) => api<SettingOnResponse>("/admin/settings/guardian-copies", { method: "PUT", body: { on } }),
  // Notifications on devices for the whole church (DEC-124, default on).
  pushEnabled: () => api<SettingOnResponse>("/admin/settings/push-enabled"),
  setPushEnabled: (on: boolean) => api<SettingOnResponse>("/admin/settings/push-enabled", { method: "PUT", body: { on } }),
  replyTo: () => api<SettingAddressResponse>("/admin/settings/reply-to"),
  setReplyTo: (address: string, revision: string | undefined) => api<SettingAddressResponse>("/admin/settings/reply-to", { method: "PUT", body: { address, revision } }),
};

export const AdminGroups = {
  list: () => api<GroupListResponse>("/admin/groups"),
  addMember: (id: Id, target: number | { selectionId: string }, role: "leader" | "member" = "member", overLimit = false) =>
    api<unknown>(path`/admin/groups/${id}/members`, { method: "POST", body: { ...(isNumber(target) ? { userId: target } : target), role, overLimit } }),
};

export const AdminJobs = {
  list: () => api<AdminJobsResponse>("/admin/jobs"),
  retry: (id: Id) => api<AdminJobStatusResponse>(path`/admin/jobs/${id}/retry`, { method: "POST" }),
  dismiss: (id: Id) => api<AdminJobStatusResponse>(path`/admin/jobs/${id}/dismiss`, { method: "POST" }),
};

export const AdminBackup = {
  // Always a 200 with a state; a missing or damaged status file is something the page explains.
  status: () => api<AdminBackupStatus>("/admin/backup"),
};
