import { MantineProvider } from '@mantine/core';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { LauncherView, type LauncherState } from './LauncherView';

function render(state: Partial<LauncherState>) {
  return renderToStaticMarkup(
    <MantineProvider>
      <LauncherView state={{ mode: 'updating', stage: 'downloading', message: '업데이트 다운로드 중', version: '1.4.3', ...state }}
        onRetry={() => undefined} onQuit={() => undefined} onDownloadLauncher={() => undefined} />
    </MantineProvider>,
  );
}

describe('Launcher status presentation', () => {
  it('shows one status heading, the component, and measured progress', () => {
    const html = render({ artifact: 'client', total: 100, downloaded: 38 });
    expect(html).toMatch(/<h1[^>]*>업데이트 다운로드 중<\/h1>/);
    expect(html).not.toContain('>업데이트 중<');
    expect(html).toContain('>Cineko Client<');
    expect(html).toContain('>38%<');
    expect(html).not.toContain('완료될 때까지');
  });

  it.each(['checking', 'launching'] as const)('does not repeat the %s status', (mode) => {
    const html = render({ mode, stage: mode, message: 'Cineko 시작 준비 중' });
    expect(html).toMatch(/<h1[^>]*>Cineko 시작 준비 중<\/h1>/);
    expect(html).not.toContain('>시작 준비 중<');
    expect(html).not.toContain('>Cineko Client<');
    expect(html).not.toContain('>릴리스 확인<');
  });

  it.each([{ total: 0 }, { total: -1 }, { stage: 'installing', total: 100, downloaded: 100 }])('does not invent a percentage: %j', (state) => {
    expect(render(state)).not.toMatch(/>\d+%<\/p>/);
  });

  it('does not repeat the installed version or update instructions', () => {
    const html = render({ mode: 'launcher-update', latestVersion: '1.4.4', message: '계속하려면 새 Launcher를 내려받아 실행하세요.' });
    expect(html.split('v1.4.3')).toHaveLength(2);
    expect(html).not.toContain('계속하려면');
    expect(html).toContain('새 버전을 다운로드한 뒤 기존 앱을 교체하세요.');
  });
});
