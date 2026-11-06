// The browser still owns fetching and decoding. This controller only waits for
// a modest amount of buffered media before starting or recovering from a stall.
const TARGET_SECONDS = 10;
const MAX_WAIT_MS = 8000;

export function bufferedAhead(video: HTMLVideoElement): number {
  for (let i = 0; i < video.buffered.length; i++) {
    if (
      video.buffered.start(i) <= video.currentTime + 0.05 &&
      video.buffered.end(i) >= video.currentTime
    ) {
      return Math.max(0, video.buffered.end(i) - video.currentTime);
    }
  }
  return 0;
}

export function bufferVideo(
  video: HTMLVideoElement,
  onBuffering: (active: boolean) => void,
  maxWaitMs = MAX_WAIT_MS,
) {
  let buffering = false;
  let disposed = false;
  let skipPlay = false;
  let bypassNextPlay = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let lastGatePosition = Number.NEGATIVE_INFINITY;

  const clear = () => {
    if (!buffering) return;
    buffering = false;
    clearTimeout(timer);
    timer = undefined;
    onBuffering(false);
  };
  const resume = () => {
    if (!buffering) return;
    clear();
    if (disposed) return;
    skipPlay = true;
    void video.play().catch(() => {
      skipPlay = false;
      // Some browsers require a fresh user gesture after the wait.
      bypassNextPlay = true;
    });
  };
  const enough = () => {
    const remaining = Number.isFinite(video.duration)
      ? Math.max(0, video.duration - video.currentTime)
      : TARGET_SECONDS;
    return bufferedAhead(video) >= Math.min(TARGET_SECONDS, remaining) && video.readyState >= 3;
  };
  const check = () => {
    if (buffering && enough()) resume();
  };
  const begin = () => {
    if (buffering || disposed || video.ended || enough()) return;
    buffering = true;
    lastGatePosition = video.currentTime;
    video.pause();
    onBuffering(true);
    timer = setTimeout(resume, maxWaitMs);
    check();
  };
  const onPlay = () => {
    if (bypassNextPlay) {
      bypassNextPlay = false;
      return;
    }
    if (skipPlay) {
      skipPlay = false;
      return;
    }
    // If the user presses the player's play button while waiting, honour it.
    if (buffering) {
      clear();
      return;
    }
    begin();
  };
  const onWaiting = () => {
    // Avoid repeatedly pausing at the same position after the timeout.
    if (!video.paused && Math.abs(video.currentTime - lastGatePosition) >= 1) begin();
  };
  const onSeeking = () => {
    if (buffering) {
      lastGatePosition = video.currentTime;
      check();
    }
  };
  const onError = () => clear();
  video.preload = "auto";
  video.addEventListener("play", onPlay);
  video.addEventListener("waiting", onWaiting);
  video.addEventListener("progress", check);
  video.addEventListener("canplay", check);
  video.addEventListener("seeked", check);
  video.addEventListener("seeking", onSeeking);
  video.addEventListener("error", onError);
  video.addEventListener("ended", onError);

  return {
    playNow: resume,
    pause: clear,
    dispose() {
      disposed = true;
      clear();
      video.removeEventListener("play", onPlay);
      video.removeEventListener("waiting", onWaiting);
      video.removeEventListener("progress", check);
      video.removeEventListener("canplay", check);
      video.removeEventListener("seeked", check);
      video.removeEventListener("seeking", onSeeking);
      video.removeEventListener("error", onError);
      video.removeEventListener("ended", onError);
    },
  };
}
