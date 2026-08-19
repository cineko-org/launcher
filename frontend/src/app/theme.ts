import { Button, Paper, createTheme } from '@mantine/core';

const fontFamily = "'Pretendard Variable', Pretendard, sans-serif";

export const cinekoTheme = createTheme({
  primaryColor: 'cineko',
  defaultRadius: 'xs',
  radius: { xs: '0px', sm: '0px', md: '0px', lg: '0px', xl: '0px' },
  fontFamily,
  headings: { fontFamily, fontWeight: '700', textWrap: 'balance' },
  colors: { cineko: ['#fff0f0', '#ffdddd', '#ffc0c1', '#ff9a9d', '#ff7478', '#ff5a5f', '#f13f45', '#d72e34', '#bd2228', '#a5141a'] },
  components: {
    Button: Button.extend({ styles: { root: { borderRadius: 0 } } }),
    Paper: Paper.extend({ styles: { root: { borderRadius: 0 } } }),
  },
});
