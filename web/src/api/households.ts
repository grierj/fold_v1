import type {
  HouseholdConversionResponse,
  HouseholdPerson,
  HouseholdPersonResponse,
  HouseholdResponse,
  MyHouseholdResponse,
} from "../types";
import { api, path, type Credential, type Id } from "./core";

export interface HouseholdPersonInput {
  firstName: string;
  lastName?: string;
  kind: HouseholdPerson["kind"];
  birthDate?: string;
  careNotes?: string;
  photoConsent?: boolean;
}

// GET never creates: it answers {household: null} until the household
// exists, and always for teen accounts (Warden, PR 106).
export const Households = {
  convert: (id: Id, email: string) =>
    api<HouseholdConversionResponse>(path`/me/household/people/${id}/convert`, { method: "POST", body: { email } }),
  resendConversion: (id: Id) =>
    api<HouseholdConversionResponse>(path`/me/household/people/${id}/convert/resend`, { method: "POST" }),
  cancelConversion: (id: Id) => api<Record<string, never>>(path`/me/household/people/${id}/convert`, { method: "DELETE" }),
  // pinned (M8.1): a participating read, answered only for this credential (api/core.ts).
  mine: (pinned?: Credential) => api<MyHouseholdResponse>("/me/household", { pinned }),
  // A save of the details or of a person names the read it was made from (DEC-174): an older one is 409
  // household_changed or household_person_changed.
  update: (data: { name?: string; address?: string }, revision: string | undefined) =>
    api<HouseholdResponse>("/me/household", { method: "PUT", body: { ...data, revision: revision ?? "" } }),
  addPerson: (data: HouseholdPersonInput) => api<HouseholdPersonResponse>("/me/household/people", { method: "POST", body: data }),
  updatePerson: (id: Id, data: HouseholdPersonInput, revision: string | undefined) =>
    api<HouseholdPersonResponse>(path`/me/household/people/${id}`, { method: "PUT", body: { ...data, revision: revision ?? "" } }),
  removePerson: (id: Id) => api<Record<string, never>>(path`/me/household/people/${id}`, { method: "DELETE" }),
};
