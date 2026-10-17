import { Button, Checkbox, Input, Label, Spinner, TextField } from "@heroui/react";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import CopyIcon from "~icons/gravity-ui/copy";
import { copyText } from "./download";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { AppDialog } from "@/components/dialogs/app-dialog";
import { useAppLocale } from "@/lib/locale";

type ScanItem = {
  messageId: number;
  name: string;
  size: number;
  mimeType?: string;
  modTime: string;
  imported: boolean;
  link: string;
};

type ImportDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  parentId?: string;
  parentPath: string;
};

function parseChannelLink(value: string): { channelId: number; messageId: number } | null {
  const match = value.match(/t\.me\/c\/(\d+)\/(\d+)/i);
  if (!match) return null;
  const channelId = Number(match[1]);
  const messageId = Number(match[2]);
  if (!Number.isSafeInteger(channelId) || !Number.isSafeInteger(messageId)) return null;
  return { channelId, messageId };
}

function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let size = value / 1024;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size.toFixed(size >= 10 ? 0 : 1)} ${units[unit]}`;
}

export function ImportDialog({ open, onOpenChange, parentId, parentPath }: ImportDialogProps) {
  const { t } = useAppLocale();
  const channels = $api.useQuery("get", "/v1/channels", {
    params: { query: { limit: 200 } },
    enabled: open,
  });
  const scan = $api.useMutation("post", "/v1/files/import/scan");
  const createImport = $api.useMutation("post", "/v1/files/import");
  const [channelId, setChannelId] = useState<number>();
  const [items, setItems] = useState<ScanItem[]>([]);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [cursor, setCursor] = useState<number>();
  const [exhausted, setExhausted] = useState(true);
  const [link, setLink] = useState("");
  const [highlight, setHighlight] = useState<number>();

  useEffect(() => {
    if (!open) return;
    setItems([]);
    setSelected(new Set());
    setCursor(undefined);
    setExhausted(true);
    setLink("");
    setHighlight(undefined);
    const first = channels.data?.items?.[0]?.id;
    if (first && channelId === undefined) setChannelId(first);
  }, [open, channels.data, channelId]);

  const selectedChannel = channels.data?.items?.find((channel) => channel.id === channelId);

  const runScan = async (append: boolean) => {
    if (!channelId) return;
    try {
      const page = await scan.mutateAsync({
        body: { channelId, beforeId: append ? cursor : undefined, limit: 100 },
      });
      setItems((current) => {
        const next = append ? [...current, ...page.items] : page.items;
        const seen = new Set<number>();
        return next.filter((item) => {
          if (seen.has(item.messageId)) return false;
          seen.add(item.messageId);
          return true;
        });
      });
      setCursor(page.nextCursor ? Number(page.nextCursor) : undefined);
      setExhausted(page.exhausted);
      if (!append) {
        setSelected(
          new Set(page.items.filter((item) => !item.imported).map((item) => item.messageId)),
        );
      }
      if (highlight) {
        const found = page.items.find((item) => item.messageId === highlight);
        if (found && !found.imported) {
          setSelected((current) => new Set(current).add(found.messageId));
        }
      }
    } catch (error) {
      toast.error(t("import.scanFailed"), { description: userMessage(error) });
    }
  };

  const applyLink = () => {
    const parsed = parseChannelLink(link.trim());
    if (!parsed) {
      toast.error(t("import.invalidLink"));
      return;
    }
    const channel = channels.data?.items?.find((item) => item.id === parsed.channelId);
    if (!channel) {
      toast.error(t("import.unknownChannel"));
      return;
    }
    setChannelId(parsed.channelId);
    setHighlight(parsed.messageId);
    const found = items.find((item) => item.messageId === parsed.messageId);
    if (found) {
      if (found.imported) {
        toast.info(t("import.alreadyImported"));
      } else {
        setSelected((current) => new Set(current).add(found.messageId));
      }
      return;
    }
    toast.info(t("import.scanToFind"));
  };

  const selectable = useMemo(() => items.filter((item) => !item.imported), [items]);

  const runImport = async () => {
    if (!channelId || selected.size === 0) return;
    try {
      const job = await createImport.mutateAsync({
        body: { channelId, parentId, messageIds: [...selected] },
      });
      toast.success(t("import.started", { count: job.total }));
      onOpenChange(false);
    } catch (error) {
      toast.error(t("import.startFailed"), { description: userMessage(error) });
    }
  };

  return (
    <AppDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t("import.title")}
      description={t("import.desc", { path: parentPath || "/" })}
      size="lg"
      footer={
        <>
          <Button variant="tertiary" onPress={() => onOpenChange(false)}>
            {t("dialog.cancel")}
          </Button>
          <Button
            isPending={createImport.isPending}
            isDisabled={selected.size === 0 || !channelId}
            onPress={() => void runImport()}
          >
            {t("import.importSelected", { count: selected.size })}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1.5">
          <Label>{t("import.channel")}</Label>
          <select
            className="h-9 rounded-lg border border-border bg-surface px-2 text-sm"
            value={channelId ?? ""}
            onChange={(event) => {
              setChannelId(Number(event.target.value));
              setItems([]);
              setSelected(new Set());
              setCursor(undefined);
            }}
          >
            {(channels.data?.items ?? []).map((channel) => (
              <option key={channel.id} value={channel.id}>
                {channel.name}
              </option>
            ))}
          </select>
          {selectedChannel ? null : (
            <span className="text-xs text-muted">{t("import.noChannels")}</span>
          )}
        </div>
        <div className="flex items-end gap-2">
          <TextField className="flex-1" value={link} onChange={setLink}>
            <Label>{t("import.linkLabel")}</Label>
            <Input placeholder="https://t.me/c/1234567890/42" />
          </TextField>
          <Button variant="secondary" onPress={applyLink} isDisabled={!link.trim()}>
            {t("import.useLink")}
          </Button>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            isPending={scan.isPending}
            isDisabled={!channelId}
            onPress={() => void runScan(false)}
          >
            {t("import.scan")}
          </Button>
          {items.length > 0 ? (
            <Button
              variant="tertiary"
              isDisabled={exhausted || cursor === undefined || scan.isPending}
              onPress={() => void runScan(true)}
            >
              {t("import.loadMore")}
            </Button>
          ) : null}
          <span className="text-xs text-muted">
            {t("import.selectable", { count: selectable.length })}
          </span>
        </div>
        <div className="max-h-[45vh] overflow-y-auto rounded-lg border border-border">
          {scan.isPending && items.length === 0 ? (
            <div className="flex justify-center py-8">
              <Spinner />
            </div>
          ) : items.length === 0 ? (
            <div className="p-4 text-sm text-muted">{t("import.empty")}</div>
          ) : (
            <ul className="divide-y divide-border">
              {items.map((item) => (
                <li
                  key={item.messageId}
                  className={
                    highlight === item.messageId
                      ? "flex items-center gap-3 bg-accent/10 px-3 py-2"
                      : "flex items-center gap-3 px-3 py-2"
                  }
                >
                  <Checkbox
                    isSelected={selected.has(item.messageId)}
                    isDisabled={item.imported}
                    onChange={(checked) =>
                      setSelected((current) => {
                        const next = new Set(current);
                        if (checked) next.add(item.messageId);
                        else next.delete(item.messageId);
                        return next;
                      })
                    }
                  >
                    <Checkbox.Content>
                      <Checkbox.Control>
                        <Checkbox.Indicator />
                      </Checkbox.Control>
                    </Checkbox.Content>
                  </Checkbox>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm">{item.name}</div>
                    <div className="text-xs text-muted">
                      {formatBytes(item.size)} · #{item.messageId}
                      {item.imported ? ` · ${t("import.indexed")}` : ""}
                    </div>
                  </div>
                  <Button
                    isIconOnly
                    size="sm"
                    variant="ghost"
                    aria-label={t("import.copyLink")}
                    onPress={() => void copyText(item.link)}
                  >
                    <CopyIcon className="size-4" />
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
        <p className="text-xs text-muted">{t("import.hint")}</p>
      </div>
    </AppDialog>
  );
}
