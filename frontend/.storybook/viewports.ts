import type { ViewportMap } from 'storybook/viewport';

export const cinekoViewports: ViewportMap = {
  phone: { name: 'Phone 360 × 800', styles: { width: '360px', height: '800px' }, type: 'mobile' },
  launcher: { name: 'Launcher 720 × 560', styles: { width: '720px', height: '560px' }, type: 'desktop' },
  desktop: { name: 'Desktop 1440 × 900', styles: { width: '1440px', height: '900px' }, type: 'desktop' },
};
