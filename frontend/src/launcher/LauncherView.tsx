import type { ReactNode } from 'react';
import { Alert, Box, Button, Group, Progress, SimpleGrid, Stack, Text, Title } from '@mantine/core';

export type LauncherMode = 'checking' | 'updating' | 'launcher-update' | 'launching' | 'error';

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
  onRetry: () => void;
  onQuit: () => void;
  onDownloadLauncher: () => void;
}

const artifactLabel: Record<string, string> = { launcher: 'Cineko Launcher', client: 'Cineko Client', browser: 'Chromium', playwright: 'Playwright' };

function Shell({ state, children }: { state: LauncherState; children: ReactNode }) {
  return (
    <Box mih="100dvh" bg="dark.9" px={{ base: 20, sm: 48, lg: 72 }} py={{ base: 28, sm: 40 }}>
      <Stack w="100%" maw={1040} mx="auto" gap={0}>
        <Group justify="space-between" h={24}>
          <Text size="xs" c="dimmed" tt="uppercase" fw={700} lts="0.12em">Cineko Launcher</Text>
          <Text size="xs" c="dimmed">v{state.version}</Text>
        </Group>
        <Stack mt="clamp(88px, 16dvh, 136px)" gap={28} maw={760}>
          {children}
        </Stack>
      </Stack>
    </Box>
  );
}

function Heading({ title, message }: { title: string; message?: string }) {
  return (
    <Stack gap={6} align="flex-start" maw={760}>
      <Title order={1} fz={{ base: 28, sm: 36 }}>{title}</Title>
      {message && <Text c="dimmed" fz={{ base: 'sm', sm: 'md' }}>{message}</Text>}
    </Stack>
  );
}

function UpdateView({ state }: { state: LauncherState }) {
  const measurable = state.stage === 'downloading' && (state.total ?? 0) > 0;
  const percent = measurable ? Math.max(0, Math.min(100, Math.round(((state.downloaded ?? 0) / state.total!) * 100))) : 100;
  const artifact = artifactLabel[state.artifact ?? ''];
  return (
    <Shell state={state}>
      <Heading title={state.message} />
      <Stack gap="sm">
        <Progress value={percent} animated={!measurable} aria-label={state.message} />
        {(artifact || measurable) && <Group justify="space-between">
          <Text size="sm" c="dimmed">{artifact && !state.message.includes(artifact) ? artifact : ''}</Text>
          {measurable && <Text size="sm" c="dimmed">{percent}%</Text>}
        </Group>}
      </Stack>
    </Shell>
  );
}

export function LauncherView(props: LauncherViewProps) {
  const { state } = props;
  if (state.mode === 'checking') {
    return <Shell state={state}><Heading title={state.message} /><Progress value={100} animated aria-label={state.message} /></Shell>;
  }
  if (state.mode === 'updating' || state.mode === 'launching') return <UpdateView state={state} />;
  if (state.mode === 'launcher-update') {
    return (
      <Shell state={state}>
        <Heading title="Launcher 업데이트 필요" message="새 버전을 다운로드한 뒤 기존 앱을 교체하세요." />
        <Text>새 버전 v{state.latestVersion}</Text>
        <Stack gap="sm">
          <Button onClick={props.onDownloadLauncher}>새 Launcher 다운로드</Button>
          <Button variant="default" onClick={props.onQuit}>종료</Button>
        </Stack>
      </Shell>
    );
  }
  if (state.mode === 'error') {
    return (
      <Shell state={state}>
        <Heading title="시작할 수 없음" />
        <Alert color="red">{state.message}</Alert>
        <SimpleGrid cols={{ base: 1, xs: 2 }}><Button variant="default" onClick={props.onQuit}>종료</Button><Button onClick={props.onRetry}>다시 시도</Button></SimpleGrid>
      </Shell>
    );
  }
  return <Shell state={state}><Heading title={state.message} /><Progress value={100} animated aria-label={state.message} /></Shell>;
}
