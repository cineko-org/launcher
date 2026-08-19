import '@mantine/core/styles.css';
import 'pretendard/dist/web/variable/pretendardvariable.css';
import type { Preview } from '@storybook/react-vite';
import { MantineProvider } from '@mantine/core';
import { cinekoTheme } from '../src/app/theme';

const preview: Preview = {
  parameters: { layout: 'fullscreen', backgrounds: { disable: true }, options: { showPanel: false } },
  decorators: [(Story) => <MantineProvider forceColorScheme="dark" theme={cinekoTheme}><Story /></MantineProvider>],
};

export default preview;
