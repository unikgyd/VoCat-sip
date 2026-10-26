export interface RecordingConfig { enabled: boolean; keepCalls: number; maxMinutes: number; maxStorageMib: number }
export interface RecordingStatus { config: RecordingConfig; directory: string; usedBytes: number; active: number; lastError?: string }
export interface CallRecording {
  id: string; deviceId: string; iccid: string; callId: string; number: string; direction: string; access: string;
  startedAt: string; endedAt?: string; durationMs: number; bytes: number; state: string; stopReason?: string;
  droppedSamples: number; uplinkSamples: number; downlinkSamples: number;
}
export const recordingAudio = (id: string) => `/api/recordings/${encodeURIComponent(id)}/audio`;
