import { getJSON, postJSON, deleteJSON } from "./client";

export type PGPIdentity = {
  fingerprint: string;
  keyId: string;
  publicKey: string;
  source: "generated" | "imported";
  createdAt: string;
};

export type PGPRecipientStatus = {
  address: string;
  hasKey: boolean;
};

export function getPGPIdentity(): Promise<PGPIdentity> {
  return getJSON<PGPIdentity>("/api/pgp/identity");
}

export function generatePGPIdentity(): Promise<PGPIdentity> {
  return postJSON<PGPIdentity>("/api/pgp/identity/generate", {});
}

export function importPGPIdentity(armoredPrivateKey: string, passphrase: string): Promise<PGPIdentity> {
  return postJSON<PGPIdentity>("/api/pgp/identity/import", { armoredPrivateKey, passphrase });
}

export function deletePGPIdentity(): Promise<{ ok: boolean }> {
  return deleteJSON<{ ok: boolean }>("/api/pgp/identity");
}

export function checkPGPRecipients(addresses: string[]): Promise<{ results: PGPRecipientStatus[] }> {
  return postJSON<{ results: PGPRecipientStatus[] }>("/api/pgp/recipients/check", { addresses });
}

export function lookupPGPKeyserver(
  email: string
): Promise<{ email: string; fingerprint: string; keyId: string; publicKey: string; revoked: boolean; expired: boolean }> {
  return getJSON(`/api/pgp/keyserver/lookup?email=${encodeURIComponent(email)}`);
}
