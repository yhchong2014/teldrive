import { apiFetch } from "@/api/client";
import type { FileEntry } from "@/api/types";

export type PlayerProvider = "vlc" | "potplayer";

export type StreamToken = {
  path: string;
  token: string;
  expiresAt: string;
  sessionId: string;
  accountId?: number;
};

/** Issue a short-lived signed URL that VLC/PotPlayer can open directly. */
export async function createStreamToken(fileId: string): Promise<StreamToken> {
  const response = await apiFetch(`/v1/files/${encodeURIComponent(fileId)}/stream-token`, {
    method: "POST",
  });
  return (await response.json()) as StreamToken;
}

export function absoluteStreamUrl(path: string) {
  return new URL(path, window.location.origin).toString();
}

/** VLC and PotPlayer accept a full HTTP URL after their custom scheme. */
export function playerUrl(provider: PlayerProvider, url: string) {
  return `${provider}://${url}`;
}

export function fileContentUrl(file: Pick<FileEntry, "id" | "name">, download = false) {
  const path = `/api/v1/files/${encodeURIComponent(file.id)}/content/${encodeURIComponent(file.name)}`;
  return download ? `${path}?download=1` : path;
}

export function startFileDownload(file: Pick<FileEntry, "id" | "name">) {
  const anchor = document.createElement("a");
  anchor.href = fileContentUrl(file, true);
  anchor.download = file.name;
  anchor.click();
}

export async function copyText(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    return;
  } catch {
    // Clipboard API requires a secure context; the textarea path also works on HTTP IP hosts.
  }

  const activeElement =
    document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  activeElement?.focus();
  if (!copied) throw new Error("Clipboard access is unavailable");
}

export function absoluteFileDownloadUrl(file: Pick<FileEntry, "id" | "name">) {
  return new URL(fileContentUrl(file, true), window.location.origin).toString();
}
