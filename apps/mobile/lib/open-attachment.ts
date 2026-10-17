/**
 * Open an attachment outside the app (Safari / QuickLook / share sheet).
 *
 * Why this exists: list and upload responses hand out the auth-gated
 * `/api/attachments/{id}/download` path, and `Linking.openURL` starts a
 * request that carries neither the Bearer token nor a session cookie — the
 * server answers 401 `{"error":"missing authorization"}` and the user sees
 * an error page instead of their file.
 *
 * The fix mirrors web's `useDownloadAttachment` (packages/views/editor/
 * use-download-attachment.ts): re-fetch the record through the
 * authenticated metadata endpoint, which re-signs `download_url` on every
 * call — a CloudFront / S3 presigned URL, or in proxy mode a short-lived
 * single-attachment capability that authenticates itself in the query
 * string. Then hand THAT to the system opener.
 *
 * URLs that carry no attachment id (public CDN links, external links,
 * capability URLs the server already made credential-free) open unchanged.
 */
import { Alert, Linking } from "react-native";
import { attachmentIdFromDownloadURL } from "@multica/core/types";
import { api } from "@/data/api";
import { i18n } from "@/lib/i18n/singleton";
import { resolveAttachmentUrl } from "./attachment-url";

export async function openAttachment(
  rawUrl: string | null | undefined,
): Promise<void> {
  if (!rawUrl) return;

  const attachmentId = attachmentIdFromDownloadURL(rawUrl);
  let target = resolveAttachmentUrl(rawUrl);

  if (attachmentId) {
    try {
      const fresh = await api.getAttachmentForDownload(attachmentId);
      // `id: ""` is the parse fallback — keep the original URL rather than
      // opening an empty string.
      if (fresh.id) target = resolveAttachmentUrl(fresh.download_url) ?? target;
    } catch {
      // Offline or a server that predates the endpoint. The stored URL may
      // still be a public CDN path, so fall through and try it.
    }
  }

  if (!target) return;
  try {
    await Linking.openURL(target);
  } catch {
    Alert.alert(i18n.t("common:attachments.open_failed"));
  }
}
