import type { Meta, StoryObj } from '@storybook/react-vite';
import { LauncherView } from './LauncherView';

const meta = {
  title: 'Launcher/Application',
  component: LauncherView,
  args: {
    state: { mode: 'checking', message: 'Cineko 시작 준비 중', version: '1.0.0' },
    onRetry: () => undefined,
    onQuit: () => undefined,
    onDownloadLauncher: () => undefined,
  },
} satisfies Meta<typeof LauncherView>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Checking: Story = { args: { state: { mode: 'checking', message: '설치된 Client 확인 중', version: '1.0.0' } } };
export const DownloadingClient: Story = { args: { state: { mode: 'updating', stage: 'downloading', message: '업데이트 다운로드 중', artifact: 'client', downloaded: 38, total: 100, version: '1.0.0' } } };
export const LauncherUpdateRequired: Story = { args: { state: { mode: 'launcher-update', message: '계속하려면 새 Launcher를 내려받아 실행하세요.', version: '1.0.0', latestVersion: '1.1.0', downloadUrl: 'https://releases.example.com/cineko/launcher/v1.1.0/darwin-arm64/cineko-launcher-v1.1.0-darwin-arm64.zip' } } };
export const InstallingChromium: Story = { args: { state: { mode: 'updating', stage: 'installing', message: '다운로드 검증 및 설치 중', artifact: 'browser', version: '1.0.0' } } };
export const LaunchingClient: Story = { args: { state: { mode: 'launching', stage: 'launching', message: 'Cineko Client 시작 중', version: '1.0.0' } } };
export const ReleaseServerUnavailable: Story = { args: { state: { mode: 'error', message: '업데이트 서버에 연결할 수 없습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요.', version: '1.0.0' } } };
export const MobileError: Story = {
  globals: { viewport: { value: 'phone', isRotated: false } },
  args: { state: { mode: 'error', message: 'Cineko Client를 시작할 수 없습니다.', version: '1.0.0' } },
};
