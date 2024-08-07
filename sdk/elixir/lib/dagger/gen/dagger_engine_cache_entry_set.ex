# This file generated by `dagger_codegen`. Please DO NOT EDIT.
defmodule Dagger.DaggerEngineCacheEntrySet do
  @moduledoc "A set of cache entries returned by a query to a cache"

  use Dagger.Core.QueryBuilder

  @derive Dagger.ID

  defstruct [:selection, :client]

  @type t() :: %__MODULE__{}

  @doc "The total disk space used by the cache entries in this set."
  @spec disk_space_bytes(t()) :: {:ok, integer()} | {:error, term()}
  def disk_space_bytes(%__MODULE__{} = dagger_engine_cache_entry_set) do
    selection =
      dagger_engine_cache_entry_set.selection |> select("diskSpaceBytes")

    execute(selection, dagger_engine_cache_entry_set.client)
  end

  @doc "The list of individual cache entries in the set"
  @spec entries(t()) :: {:ok, [Dagger.DaggerEngineCacheEntry.t()]} | {:error, term()}
  def entries(%__MODULE__{} = dagger_engine_cache_entry_set) do
    selection =
      dagger_engine_cache_entry_set.selection |> select("entries") |> select("id")

    with {:ok, items} <- execute(selection, dagger_engine_cache_entry_set.client) do
      {:ok,
       for %{"id" => id} <- items do
         %Dagger.DaggerEngineCacheEntry{
           selection:
             query()
             |> select("loadDaggerEngineCacheEntryFromID")
             |> arg("id", id),
           client: dagger_engine_cache_entry_set.client
         }
       end}
    end
  end

  @doc "The number of cache entries in this set."
  @spec entry_count(t()) :: {:ok, integer()} | {:error, term()}
  def entry_count(%__MODULE__{} = dagger_engine_cache_entry_set) do
    selection =
      dagger_engine_cache_entry_set.selection |> select("entryCount")

    execute(selection, dagger_engine_cache_entry_set.client)
  end

  @doc "A unique identifier for this DaggerEngineCacheEntrySet."
  @spec id(t()) :: {:ok, Dagger.DaggerEngineCacheEntrySetID.t()} | {:error, term()}
  def id(%__MODULE__{} = dagger_engine_cache_entry_set) do
    selection =
      dagger_engine_cache_entry_set.selection |> select("id")

    execute(selection, dagger_engine_cache_entry_set.client)
  end
end
