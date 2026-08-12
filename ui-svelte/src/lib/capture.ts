/** Keep the displayed capture ID identical to the ID accepted by the API. */
export function displayCaptureId(id: number): string {
  return String(id);
}

export function captureTitle(id: number): string {
  return `Capture #${displayCaptureId(id)}`;
}
