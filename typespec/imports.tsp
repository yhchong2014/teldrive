using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

enum ImportJobStatus {
  pending: "pending",
  running: "running",
  completed: "completed",
  failed: "failed",
}

model ImportScanRequest {
  /** Teldrive Telegram channel that contains the forwarded files. */
  channelId: int64;

  /** Pagination cursor: only messages with a smaller ID are returned. */
  beforeId?: int64;

  @minValue(1)
  @maxValue(100)
  limit?: int32 = 100;
}

model ImportScanItem {
  messageId: int64;
  name: string;
  size: int64;
  mimeType?: string;
  modTime: utcDateTime;

  /** True when this Telegram message is already part of the catalog. */
  imported: boolean;

  /** Copyable Telegram link pointing at the source message. */
  link: string;
}

model ImportScanResponse {
  items: ImportScanItem[];
  nextCursor?: string;
  exhausted: boolean;
}

model ImportRequest {
  channelId: int64;

  /** Destination folder. Omit to import into the drive root. */
  parentId?: Uuid;

  @minItems(1)
  @maxItems(500)
  messageIds: int64[];
}

model ImportJob {
  id: Uuid;
  channelId: int64;
  parentId?: Uuid;
  status: ImportJobStatus;
  total: int32;
  imported: int32;
  skipped: int32;
  failed: int32;
  error?: string;
  createdAt: utcDateTime;
  updatedAt: utcDateTime;
}

model WebDAVCredentials {
  /** True when the server has WebDAV enabled in configuration. */
  enabled: boolean;
  configured: boolean;
  username?: string;
  /** URL clients should use, for example https://host/dav. */
  url?: string;
}

model WebDAVCredentialsUpdate {
  @minLength(1)
  @maxLength(64)
  username: string;

  @minLength(8)
  @maxLength(128)
  password: string;
}
