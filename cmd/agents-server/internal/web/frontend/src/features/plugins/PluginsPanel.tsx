import { Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { PlugIcon } from '@primer/octicons-react';

// A placeholder section: the nav slot exists so the shape of the settings is
// visible, but nothing here is configurable yet.
export function PluginsPanel() {
  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Plugins</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <Blankslate>
        <Blankslate.Visual><PlugIcon size={24} /></Blankslate.Visual>
        <Blankslate.Heading>Coming soon</Blankslate.Heading>
        <Blankslate.Description>
          Nothing to configure here yet. Tools and skills are set up under MCP and Skills.
        </Blankslate.Description>
      </Blankslate>
    </Stack>
  );
}

export default PluginsPanel;
