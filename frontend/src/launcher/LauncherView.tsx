import type { FormEventHandler, ReactNode } from 'react';
import { Alert, Box, Button, Center, Group, PinInput, Progress, Stack, Text, ThemeIcon, Title } from '@mantine/core';
import { IconAlertTriangle, IconCheck, IconDownload, IconLock, IconRocket } from '@tabler/icons-react';

export type LauncherMode = 'checking' | 'login' | 'updating' | 'launcher-update' | 'launching' | 'error';

export interface LauncherState {
  revision?: number;
  mode: LauncherMode;
  stage?: string;
  message: string;
  artifact?: string;
  downloaded?: number;
  total?: number;
  version: string;
  latestVersion?: string;
  downloadUrl?: string;
}

export interface LauncherViewProps {
  state: LauncherState;
  pin: string;
  connecting: boolean;
  onPinChange: (value: string) => void;
  onConnect: FormEventHandler<HTMLFormElement>;
  onRetry: () => void;
  onQuit: () => void;
  onDownloadLauncher: () => void;
}

const artifactLabel: Record<string, string> = { launcher: 'Cineko Launcher', client: 'Cineko Client', browser: 'Chromium', playwright: 'Playwright' };

function Shell({ state, children }: { state: LauncherState; children: ReactNode }) {
  return (
    <Center mih="100dvh" bg="dark.9" px={48}>
      <Stack w="100%" maw={480} gap={36}>
        <Group justify="space-between">
          <Text size="xs" c="dimmed" tt="uppercase" fw={700} lts="0.12em">Cineko Launcher</Text>
          <Text size="xs" c="dimmed">v{state.version}</Text>
        </Group>
        {children}
      </Stack>
    </Center>
  );
}

function Heading({ icon, title, message }: { icon: ReactNode; title: string; message: string }) {
  return (
    <Stack gap="md" align="flex-start">
      <ThemeIcon variant="light" color="gray" size={40}>{icon}</ThemeIcon>
      <Stack gap={6}><Title order={1} fz={32}>{title}</Title><Text c="dimmed">{message}</Text></Stack>
    </Stack>
  );
}

function UpdateView({ state }: { state: LauncherState }) {
  const percent = state.total ? Math.min(100, Math.round(((state.downloaded ?? 0) / state.total) * 100)) : 100;
  const launching = state.mode === 'launching';
  return (
    <Shell state={state}>
      <Heading icon={launching ? <IconRocket size={21} /> : <IconDownload size={21} />} title={launching ? 'Client 시작 중' : '업데이트 중'} message={state.message} />
      <Stack gap="sm">
        <Progress value={percent} animated={state.stage !== 'running'} />
        <Group justify="space-between"><Text size="sm">{artifactLabel[state.artifact ?? ''] ?? (launching ? 'Cineko Client' : '릴리스 확인')}</Text><Text size="sm" c="dimmed">{state.total ? `${percent}%` : ''}</Text></Group>
      </Stack>
      <Text size="xs" c="dimmed">완료될 때까지 Launcher를 종료하지 마세요.</Text>
    </Shell>
  );
}

export function LauncherView(props: LauncherViewProps) {
  const { state } = props;
  if (state.mode === 'checking') {
    return <Shell state={state}><Heading icon={<IconCheck size={21} />} title="시작 준비 중" message={state.message} /><Progress value={100} animated /></Shell>;
  }
  if (state.mode === 'updating' || state.mode === 'launching') return <UpdateView state={state} />;
  if (state.mode === 'launcher-update') {
    return (
      <Shell state={state}>
        <Heading icon={<IconDownload size={21} />} title="Launcher 업데이트 필요" message={state.message} />
        <Text>현재 v{state.version} · 최신 v{state.latestVersion}</Text>
        <Text size="sm" c="dimmed">Launcher는 자동으로 교체되지 않습니다. 다운로드한 무설치 Launcher를 직접 실행하세요.</Text>
        <Stack gap="sm">
          <Button leftSection={<IconDownload size={18} />} onClick={props.onDownloadLauncher}>새 Launcher 다운로드</Button>
          <Button variant="default" onClick={props.onQuit}>종료</Button>
        </Stack>
      </Shell>
    );
  }
  if (state.mode === 'error') {
    return (
      <Shell state={state}>
        <Heading icon={<IconAlertTriangle size={21} />} title="시작할 수 없음" message="Cineko Client를 준비하지 못했습니다." />
        <Alert color="red">{state.message}</Alert>
        <Group grow><Button variant="default" onClick={props.onQuit}>종료</Button><Button onClick={props.onRetry}>다시 시도</Button></Group>
      </Shell>
    );
  }
  return (
    <Shell state={state}>
      <Box component="form" onSubmit={props.onConnect}>
        <Stack gap={36}>
          <Heading icon={<IconLock size={21} />} title="기기 연결" message="관리자에게 받은 6자리 PIN을 입력하세요." />
          <Stack gap="lg">
            {state.message !== '6자리 PIN을 입력하세요' ? <Alert color="red">{state.message}</Alert> : null}
            <Stack gap="xs"><Text component="label" fw={600}>PIN</Text><PinInput length={6} type="number" size="lg" value={props.pin} onChange={props.onPinChange} autoFocus oneTimeCode aria-label="6자리 PIN" /></Stack>
            <Button type="submit" size="md" loading={props.connecting} disabled={props.pin.length !== 6} fullWidth>연결하고 시작</Button>
          </Stack>
        </Stack>
      </Box>
    </Shell>
  );
}
