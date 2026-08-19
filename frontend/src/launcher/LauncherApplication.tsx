import { type FormEvent, useEffect, useState } from 'react';
import { LauncherView, type LauncherState } from './LauncherView';

interface LauncherBridge {
  State(): Promise<LauncherState>;
  Connect(pin: string): Promise<void>;
  Retry(): Promise<void>;
  Quit(): Promise<void>;
  DownloadLauncher(): Promise<void>;
}

declare global {
  interface Window {
    go?: { desktop?: { Launcher?: LauncherBridge } };
    runtime?: { EventsOn?: (name: string, callback: (state: LauncherState) => void) => (() => void) };
  }
}

const initialState: LauncherState = { revision: 0, mode: 'checking', message: 'Cineko 시작 준비 중', version: 'dev' };

export function LauncherApplication() {
  const [state, setState] = useState<LauncherState>(initialState);
  const [pin, setPin] = useState('');
  const [connecting, setConnecting] = useState(false);
  const bridge = window.go?.desktop?.Launcher;

  useEffect(() => {
    if (!bridge) return undefined;
    const applyState = (incoming: LauncherState) => {
      setState((current) => (incoming.revision ?? 0) >= (current.revision ?? 0) ? incoming : current);
    };
    const unsubscribe = window.runtime?.EventsOn?.('launcher:state', applyState);
    void bridge.State().then(applyState);
    return unsubscribe;
  }, [bridge]);

  const connect = async (event: FormEvent) => {
    event.preventDefault();
    if (!bridge) return;
    setConnecting(true);
    try {
      await bridge.Connect(pin);
      setPin('');
    } finally {
      setConnecting(false);
    }
  };

  return <LauncherView state={state} pin={pin} connecting={connecting} onPinChange={setPin} onConnect={(event) => void connect(event)} onRetry={() => void bridge?.Retry()} onQuit={() => void bridge?.Quit()} onDownloadLauncher={() => void bridge?.DownloadLauncher()} />;
}
