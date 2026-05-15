// ConfigTemplatePicker — a dropdown that loads Squadron's curated YAML
// snippet catalog and inserts the chosen one into the editor.
//
// If the editor already has content, we ask for confirmation before
// replacing — operators have lost work to "oops I picked the wrong template
// from the menu" elsewhere.

import { ChevronDown, FileText } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import { getConfigTemplate, listConfigTemplates } from "@/api/config-tools";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { ConfigTemplate } from "@/types/config-tools";

interface ConfigTemplatePickerProps {
  currentValue: string;
  onInsert: (yaml: string) => void;
}

const TEMPLATES_KEY = "/api/v1/configs/templates";

export function ConfigTemplatePicker({
  currentValue,
  onInsert,
}: ConfigTemplatePickerProps) {
  const { data: templates, isLoading } = useSWR<ConfigTemplate[]>(
    TEMPLATES_KEY,
    listConfigTemplates,
  );
  const [open, setOpen] = useState(false);

  const handlePick = async (id: string) => {
    const hasContent = currentValue.trim().length > 0;
    if (hasContent) {
      const ok = window.confirm(
        "Replace the current editor contents with this template? Your unsaved work will be lost.",
      );
      if (!ok) return;
    }
    try {
      const tmpl = await getConfigTemplate(id);
      onInsert(tmpl.yaml);
      setOpen(false);
    } catch (e) {
      // Surface in a way operators see; alert is intentionally blunt for now.
      alert(e instanceof Error ? e.message : "failed to load template");
    }
  };

  // Group templates by category for the menu. Stable order from the API.
  const byCategory = new Map<string, ConfigTemplate[]>();
  for (const t of templates ?? []) {
    if (!byCategory.has(t.category)) byCategory.set(t.category, []);
    byCategory.get(t.category)!.push(t);
  }

  return (
    <DropdownMenu open={open} onOpenChange={setOpen}>
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          className="gap-1"
          disabled={isLoading}
        >
          <FileText className="h-3.5 w-3.5" />
          Templates
          <ChevronDown className="h-3.5 w-3.5" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-80">
        {[...byCategory.entries()].map(([category, items], idx) => (
          <DropdownMenuGroup key={category}>
            {idx > 0 && <DropdownMenuSeparator />}
            <DropdownMenuLabel className="text-xs uppercase tracking-wide text-muted-foreground">
              {category}
            </DropdownMenuLabel>
            {items.map((t) => (
              <DropdownMenuItem
                key={t.id}
                onSelect={() => handlePick(t.id)}
                className="flex flex-col items-start gap-0.5 py-2"
              >
                <span className="text-sm font-medium">{t.name}</span>
                <span className="text-xs text-muted-foreground whitespace-normal">
                  {t.description}
                </span>
              </DropdownMenuItem>
            ))}
          </DropdownMenuGroup>
        ))}
        {!isLoading && byCategory.size === 0 && (
          <div className="px-3 py-2 text-xs text-muted-foreground">
            No templates available.
          </div>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
