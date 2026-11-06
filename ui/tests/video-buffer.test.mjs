import assert from "node:assert/strict";
import { test } from "node:test";
import { bufferVideo, bufferedAhead } from "../src/components/viewers/video-buffer.ts";

class FakeVideo extends EventTarget {
  currentTime = 0;
  duration = 100;
  readyState = 2;
  paused = true;
  ended = false;
  preload = "metadata";
  ranges = [];
  get buffered() {
    return { length: this.ranges.length, start: (i) => this.ranges[i][0], end: (i) => this.ranges[i][1] };
  }
  play() { this.paused = false; this.dispatchEvent(new Event("play")); return Promise.resolve(); }
  pause() { this.paused = true; }
}

test("buffers before playback and resumes when enough data arrives", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active));
  await video.play();
  assert.equal(video.paused, true);
  assert.deepEqual(states, [true]);
  video.ranges = [[0, 12]]; video.readyState = 3;
  video.dispatchEvent(new Event("progress"));
  assert.equal(video.paused, false);
  assert.deepEqual(states, [true, false]);
  controller.dispose();
});

test("manual pause, seek and play now are respected", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active), 20);
  await video.play();
  video.currentTime = 50;
  video.dispatchEvent(new Event("seeking"));
  video.ranges = [[0, 20], [50, 53]]; video.readyState = 3;
  assert.equal(bufferedAhead(video), 3);
  video.dispatchEvent(new Event("progress"));
  assert.equal(video.paused, true);
  controller.pause();
  await new Promise((resolve) => setTimeout(resolve, 35));
  assert.equal(video.paused, true);
  await video.play();
  controller.playNow();
  assert.equal(video.paused, false);
  assert.deepEqual(states, [true, false, true, false]);
  controller.dispose();
});

test("timeout resumes once without repeatedly gating the same stall", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active), 10);
  await video.play();
  await new Promise((resolve) => setTimeout(resolve, 25));
  assert.equal(video.paused, false);
  video.dispatchEvent(new Event("waiting"));
  assert.deepEqual(states, [true, false]);
  video.currentTime = 5;
  video.dispatchEvent(new Event("waiting"));
  assert.equal(video.paused, true);
  controller.dispose();
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(video.paused, true);
});

test("handles near-end and error cleanup", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active), 10);
  video.currentTime = 98; video.ranges = [[98, 100]]; video.readyState = 3;
  await video.play();
  assert.deepEqual(states, []);
  video.currentTime = 40; video.dispatchEvent(new Event("waiting"));
  assert.deepEqual(states, [true]);
  video.dispatchEvent(new Event("error"));
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(video.paused, true);
  assert.deepEqual(states, [true, false]);
  controller.dispose();
});


test("a user play request while buffering bypasses the wait", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active));
  await video.play();
  await video.play();
  assert.equal(video.paused, false);
  assert.deepEqual(states, [true, false]);
  controller.dispose();
});

test("a rejected resume leaves controls available", async () => {
  const video = new FakeVideo(), states = [];
  const controller = bufferVideo(video, (active) => states.push(active));
  await video.play();
  const originalPlay = video.play;
  video.play = () => Promise.reject(new Error("autoplay blocked"));
  controller.playNow();
  await Promise.resolve();
  assert.equal(video.paused, true);
  assert.deepEqual(states, [true, false]);
  video.play = originalPlay;
  await video.play();
  assert.deepEqual(states, [true, false]);
  assert.equal(video.paused, false);
  controller.dispose();
});
