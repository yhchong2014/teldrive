import { VideoPlayer } from "@vplayer/react";
import "@vplayer/react/player.css";
import { useEffect, useRef, useState } from "react";
import type { FileEntry } from "@/api/types";
import { previewMedia } from "@/features/files/preview-support";
import { useAppLocale } from "@/lib/locale";
import { bufferVideo } from "./video-buffer";

export function VideoViewer({ file, url }: { file: FileEntry; url: string }) {
  const rootRef = useRef<HTMLDivElement>(null);
  const controllerRef = useRef<ReturnType<typeof bufferVideo>>(null);
  const [buffering, setBuffering] = useState(false);
  const { t } = useAppLocale();

  useEffect(() => {
    const video = rootRef.current?.querySelector("video");
    if (!video) return;
    const controller = bufferVideo(video, setBuffering);
    controllerRef.current = controller;
    // A rejected autoplay attempt leaves the normal player controls usable.
    void video.play().catch(() => {});
    return () => {
      controller.dispose();
      controllerRef.current = null;
      video.pause();
      video.removeAttribute("src");
      video.load();
    };
  }, [url]);

  return (
    <div ref={rootRef} className="flex h-full w-full items-center justify-center bg-black">
      <div className="relative w-full max-w-384">
        <VideoPlayer
          src={url}
          type={previewMedia(file)?.type || file.mimeType}
          title={file.name}
          defaultHotkeys
          persistPreferences
          className="w-full"
        />
        {buffering && (
          <div
            role="status"
            className="absolute left-3 top-3 z-20 flex items-center gap-3 rounded-md bg-black/80 px-3 py-2 text-sm text-white"
          >
            <span>{t("preview.buffering")}</span>
            <button
              type="button"
              className="underline"
              onClick={() => controllerRef.current?.playNow()}
            >
              {t("preview.playNow")}
            </button>
            <button
              type="button"
              className="underline"
              onClick={() => controllerRef.current?.pause()}
            >
              {t("preview.pauseBuffering")}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
