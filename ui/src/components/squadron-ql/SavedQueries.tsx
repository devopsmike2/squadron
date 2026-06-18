import { Trash2, Play, Loader2 } from "lucide-react";

import type { SavedQuery } from "@/api/squadron-ql";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";

interface SavedQueriesProps {
  queries: SavedQuery[];
  isLoading: boolean;
  onSelect: (query: SavedQuery) => void;
  onDelete: (query: SavedQuery) => void;
}

export function SavedQueries({
  queries,
  isLoading,
  onSelect,
  onDelete,
}: SavedQueriesProps) {
  if (isLoading) {
    return (
      <div className="flex items-center gap-2 text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading saved queries…
      </div>
    );
  }

  if (queries.length === 0) {
    return (
      <div className="rounded-lg border border-dashed p-6 text-center text-muted-foreground">
        No saved queries yet. Save a Squadron QL query from the Query tab to
        build your library.
      </div>
    );
  }

  return (
    <ScrollArea className="h-[520px] pr-4">
      <div className="space-y-3">
        {queries.map((query) => (
          <Card key={query.id} className="p-4">
            <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <h3 className="text-base font-semibold">{query.name}</h3>
                  <span className="text-xs text-muted-foreground">
                    {new Date(query.updated_at).toLocaleString()}
                  </span>
                </div>
                {query.description && (
                  <p className="text-sm text-muted-foreground">
                    {query.description}
                  </p>
                )}
                <pre className="max-w-3xl overflow-hidden text-ellipsis whitespace-pre-wrap rounded bg-muted/40 p-2 text-sm font-mono text-muted-foreground">
                  {query.query}
                </pre>
                {query.tags?.length > 0 && (
                  <div className="flex flex-wrap gap-2">
                    {query.tags.map((tag) => (
                      <Badge key={tag} variant="outline" className="text-xs">
                        {tag}
                      </Badge>
                    ))}
                  </div>
                )}
              </div>

              <div className="flex gap-2 self-start">
                <Button onClick={() => onSelect(query)}>
                  <Play className="mr-2 h-4 w-4" /> Run
                </Button>
                <Button
                  variant="ghost"
                  className="text-destructive hover:text-destructive"
                  onClick={() => onDelete(query)}
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            </div>
          </Card>
        ))}
      </div>
    </ScrollArea>
  );
}
