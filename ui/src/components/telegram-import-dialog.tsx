import { Button, Input, Label, Switch, TextArea, TextField } from "@heroui/react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { fetchClient } from "@/api/client";
import { unwrap, userMessage } from "@/api/errors";
import type { components } from "@/api/schema";
import { useAppLocale } from "@/lib/locale";
import { AppDialog } from "./dialogs/app-dialog";

type TelegramImportRequest = components["schemas"]["TelegramImportRequest"];
type TelegramImportMode = components["schemas"]["TelegramImportMode"];
type ConflictPolicy = components["schemas"]["NameConflictPolicy"];

const MODES: TelegramImportMode[] = ["auto", "copy", "reference"];

export function TelegramImportDialog({
  open,
  onOpenChange,
  currentPath,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentPath: string;
}) {
  const { t } = useAppLocale();
  const [destination, setDestination] = useState(currentPath);
  const [channel, setChannel] = useState("");
  const [scan, setScan] = useState(true);
  const [messageIds, setMessageIds] = useState("");
  const [links, setLinks] = useState("");
  const [mode, setMode] = useState<TelegramImportMode>("auto");
  const [replace, setReplace] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (open) setDestination(currentPath);
  }, [currentPath, open]);

  const reset = () => {
    setChannel("");
    setScan(true);
    setMessageIds("");
    setLinks("");
    setMode("auto");
    setReplace(false);
  };

  const close = () => {
    if (submitting) return;
    onOpenChange(false);
  };

  const queueImport = async () => {
    const target = destination.trim();
    const source = channel.trim();
    const ids = messageIds.trim();
    const linkList = lines(links);
    if (!target) {
      toast.error(t("uploads.destRequired"));
      return;
    }
    if (!target.startsWith("/") && !isUUID(target)) {
      toast.error(t("uploads.destInvalid"));
      return;
    }
    if (!source && !linkList.length) {
      toast.error(t("telegramImport.nothing"));
      return;
    }
    if (source && !scan && !ids) {
      toast.error(t("telegramImport.channelNeedsWork"));
      return;
    }
    const policy: ConflictPolicy = replace ? "replace" : "rename";
    const body: TelegramImportRequest = {
      destination: target,
      channel: source || undefined,
      scan: Boolean(source) && scan,
      messageIds: source && ids ? [ids] : undefined,
      links: linkList.length ? linkList : undefined,
      mode,
      conflictPolicy: policy,
    };
    setSubmitting(true);
    try {
      await unwrap(fetchClient.POST("/v1/uploads/telegram-imports", { body }));
      toast.success(t("telegramImport.queued"));
      reset();
      onOpenChange(false);
    } catch (error) {
      toast.error(userMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <AppDialog
      open={open}
      onOpenChange={(next) => (next ? onOpenChange(true) : close())}
      isDismissable={!submitting}
      title={t("telegramImport.title")}
      description={t("telegramImport.description")}
      className="min-w-0 sm:w-[min(94vw,42rem)] sm:max-w-none bg-surface"
      bodyClassName="p-0"
      footer={
        <>
          <Button variant="secondary" isDisabled={submitting} onPress={close}>
            {t("dialog.cancel")}
          </Button>
          <Button variant="primary" isPending={submitting} onPress={() => void queueImport()}>
            {t("telegramImport.submit")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-4 sm:p-5">
        <TextField value={destination} onChange={setDestination} isRequired>
          <Label>{t("uploads.destination")}</Label>
          <Input placeholder="/Telegram/Movies" />
          <div className="mt-1 text-xs text-muted">{t("telegramImport.destinationHint")}</div>
        </TextField>

        <section className="grid gap-3 rounded-xl border border-border bg-default/15 p-3 sm:p-4">
          <div className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            {t("telegramImport.channelSection")}
          </div>
          <TextField value={channel} onChange={setChannel}>
            <Label>{t("telegramImport.channel")}</Label>
            <Input placeholder="-1001234567890, @channel or https://t.me/c/1234567890" />
          </TextField>
          <Switch isSelected={scan} onChange={setScan} isDisabled={!channel.trim()}>
            <Switch.Content>
              <Switch.Control>
                <Switch.Thumb />
              </Switch.Control>
              <Label>{t("telegramImport.scan")}</Label>
            </Switch.Content>
          </Switch>
          <TextField value={messageIds} onChange={setMessageIds} isDisabled={!channel.trim()}>
            <Label>{t("telegramImport.messageIds")}</Label>
            <Input placeholder="12, 15-20" />
            <div className="mt-1 text-xs text-muted">{t("telegramImport.messageIdsHint")}</div>
          </TextField>
        </section>

        <TextField>
          <Label>{t("telegramImport.links")}</Label>
          <TextArea
            value={links}
            onChange={(event) => setLinks(event.currentTarget.value)}
            placeholder={"https://t.me/c/1234567890/45\nhttps://t.me/channel/88-95"}
            rows={4}
          />
          <div className="mt-1 text-xs text-muted">{t("telegramImport.linksHint")}</div>
        </TextField>

        <div className="grid gap-2">
          <Label>{t("telegramImport.mode")}</Label>
          <div className="grid grid-cols-3 rounded-lg bg-default/35 p-1">
            {MODES.map((value) => (
              <Button
                key={value}
                size="sm"
                variant={mode === value ? "secondary" : "ghost"}
                onPress={() => setMode(value)}
              >
                {t(`telegramImport.mode.${value}`)}
              </Button>
            ))}
          </div>
          <div className="text-xs text-muted">{t(`telegramImport.modeHint.${mode}`)}</div>
        </div>

        <Switch isSelected={replace} onChange={setReplace}>
          <Switch.Content>
            <Switch.Control>
              <Switch.Thumb />
            </Switch.Control>
            <Label>{t("telegramImport.replace")}</Label>
          </Switch.Content>
        </Switch>
      </div>
    </AppDialog>
  );
}

function lines(value: string) {
  return value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
}

function isUUID(value: string) {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}
