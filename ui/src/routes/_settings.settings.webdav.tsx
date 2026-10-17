import { Button, Input, Label, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { $api } from "@/api/client";
import { userMessage } from "@/api/errors";
import { SettingsPageHeader, SettingsRow, SettingsSection } from "@/components/settings-layout";
import { copyText } from "@/features/files/download";
import { getQueryClient } from "@/lib/queryClient";
import { useAppLocale } from "@/lib/locale";

export const Route = createFileRoute("/_settings/settings/webdav")({ component: WebDAVSettings });

function WebDAVSettings() {
  const { t } = useAppLocale();
  const query = $api.useQuery("get", "/v1/settings/webdav");
  const save = $api.useMutation("put", "/v1/settings/webdav");
  const remove = $api.useMutation("delete", "/v1/settings/webdav");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  useEffect(() => {
    if (query.data?.username) setUsername(query.data.username);
  }, [query.data?.username]);

  const refresh = () =>
    getQueryClient().invalidateQueries({
      queryKey: $api.queryOptions("get", "/v1/settings/webdav").queryKey,
    });

  const url = query.data?.url
    ? new URL(query.data.url, window.location.origin).toString()
    : `${window.location.origin}/dav`;

  return (
    <div className="space-y-6">
      <SettingsPageHeader title={t("webdav.title")} description={t("webdav.desc")} />
      {query.data && !query.data.enabled ? (
        <div className="rounded-lg border border-warning/40 bg-warning/10 px-4 py-3 text-sm">
          {t("webdav.disabled")}
        </div>
      ) : null}
      <SettingsSection title={t("webdav.status")} description={t("webdav.statusDesc")}>
        <SettingsRow label={t("webdav.url")} description={t("webdav.urlDesc")}>
          <div className="flex items-center gap-2">
            <code className="rounded bg-surface-secondary px-2 py-1 text-xs">{url}</code>
            <Button size="sm" variant="secondary" onPress={() => void copyText(url)}>
              {t("import.copyLink")}
            </Button>
          </div>
        </SettingsRow>
        <SettingsRow label={t("webdav.username")} description={t("webdav.configured")}>
          <TextField value={username} onChange={setUsername} className="w-56">
            <Label className="sr-only">{t("webdav.username")}</Label>
            <Input autoComplete="username" />
          </TextField>
        </SettingsRow>
        <SettingsRow label={t("webdav.password")} description={t("webdav.notConfigured")}>
          <TextField value={password} onChange={setPassword} className="w-56">
            <Label className="sr-only">{t("webdav.password")}</Label>
            <Input type="password" autoComplete="new-password" />
          </TextField>
        </SettingsRow>
        <div className="flex flex-wrap items-center gap-2 p-4">
          <Button
            isPending={save.isPending}
            isDisabled={!username.trim() || password.length < 8}
            onPress={async () => {
              try {
                await save.mutateAsync({ body: { username: username.trim(), password } });
                setPassword("");
                await refresh();
                toast.success(t("webdav.saved"));
              } catch (error) {
                toast.error(t("webdav.saveFailed"), { description: userMessage(error) });
              }
            }}
          >
            {t("webdav.save")}
          </Button>
          {query.data?.configured ? (
            <Button
              variant="danger"
              isPending={remove.isPending}
              onPress={async () => {
                try {
                  await remove.mutateAsync({});
                  await refresh();
                  toast.success(t("webdav.removed"));
                } catch (error) {
                  toast.error(t("webdav.saveFailed"), { description: userMessage(error) });
                }
              }}
            >
              {t("webdav.remove")}
            </Button>
          ) : null}
        </div>
        <p className="px-4 pb-4 text-xs text-muted">{t("webdav.hint", { url })}</p>
      </SettingsSection>
    </div>
  );
}
