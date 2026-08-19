import type { Meta, StoryObj } from '@storybook/react-vite';
import { LauncherView } from './LauncherView';

const meta = {
  title: 'Launcher/Application',
  component: LauncherView,
  args: {
    state: { mode: 'login', message: '6자리 PIN을 입력하세요', version: '1.0.0' },
    pin: '', connecting: false, onPinChange: () => undefined,
    onConnect: (event) => event.preventDefault(), onRetry: () => undefined, onQuit: () => undefined,
    onDownloadLauncher: () => undefined,
  },
} satisfies Meta<typeof LauncherView>;

export default meta;
type Story = StoryObj<typeof meta>;
export const Login: Story = {};
export const Checking: Story = { args: { state: { mode: 'checking', message: 'Cineko 서버 연결 확인 중', version: '1.0.0' } } };
export const InvalidPIN: Story = { args: { state: { mode: 'login', message: '인증 번호가 올바르지 않습니다.', version: '1.0.0' } } };
export const DownloadingClient: Story = { args: { state: { mode: 'updating', stage: 'downloading', message: '업데이트 다운로드 중', artifact: 'client', downloaded: 38, total: 100, version: '1.0.0' } } };
export const LauncherUpdateRequired: Story = { args: { state: { mode: 'launcher-update', message: '계속하려면 새 Launcher를 내려받아 실행하세요.', version: '1.0.0', latestVersion: '1.1.0', downloadUrl: 'https://releases.example.com/cineko/launcher/v1.1.0/darwin-arm64/cineko-launcher-v1.1.0-darwin-arm64.zip' } } };
export const InstallingChromium: Story = { args: { state: { mode: 'updating', stage: 'installing', message: '다운로드 검증 및 설치 중', artifact: 'browser', version: '1.0.0' } } };
export const LaunchingClient: Story = { args: { state: { mode: 'launching', stage: 'launching', message: 'Cineko Client 시작 중', version: '1.0.0' } } };
export const ServerUnavailable: Story = { args: { state: { mode: 'error', message: '서버 응답이 없습니다. 잠시 후 다시 시도하세요.', version: '1.0.0' } } };
