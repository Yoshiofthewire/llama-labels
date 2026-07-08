/**
 * Base64url encoding helpers shared by the FCM and APNs Workers — both sign
 * JWTs (a Google service-account assertion / an APNs provider token) and need
 * the same unpadded base64url encoding for the JWT header/claims/signature.
 */

export function base64UrlEncode(bytes: ArrayBuffer | Uint8Array): string {
  const arr = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let binary = "";
  for (let i = 0; i < arr.length; i++) {
    binary += String.fromCharCode(arr[i]);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function base64UrlEncodeString(input: string): string {
  return base64UrlEncode(new TextEncoder().encode(input));
}
