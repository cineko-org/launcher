import type { StorybookConfig } from '@storybook/react-vite';

const config: StorybookConfig = {
  core: { disableWhatsNewNotifications: true, disableTelemetry: true, enableCrashReports: false },
  stories: ['../src/**/*.stories.tsx'],
  framework: { name: '@storybook/react-vite', options: {} },
  typescript: { reactDocgen: false },
};

export default config;
